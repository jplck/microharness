package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const httpPluginSource = `package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"github.com/jplck/micro/toolplugin"
)

func fetchTool() toolplugin.Tool {
	return toolplugin.Tool{
		Definition: toolplugin.Definition{
			Name: "fetch_fixture", Description: "Fetch text from a URL",
			Parameters: []toolplugin.Parameter{{Name: "url", Type: toolplugin.String, Required: true}},
		},
		Call: toolplugin.Handler(func(arguments struct { URL string ` + "`json:\"url\"`" + ` }) (string, error) {
			response, err := http.Get(arguments.URL)
			if err != nil { return "", err }
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK { return "", fmt.Errorf("HTTP status %d", response.StatusCode) }
			body, err := io.ReadAll(response.Body)
			return string(body), err
		}),
	}
}

func main() {
	if err := toolplugin.Serve([]toolplugin.Tool{fetchTool()}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
`

const httpPluginTestSource = `package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestToolSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/forecast" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, "sunny, 21 C")
	}))
	defer server.Close()
	arguments, err := json.Marshal(map[string]string{"url": server.URL + "/forecast"})
	if err != nil { t.Fatal(err) }
	result, err := fetchTool().Call(arguments)
	if err != nil || result != "sunny, 21 C" {
		t.Fatalf("successful fixture request: %q %v", result, err)
	}
}
`

func TestPluginHTTPSuccessFixture(t *testing.T) {
	env, agent, _ := setupPluginUpdateTest(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "sunny, 21 C")
	}))
	defer server.Close()
	arguments, err := json.Marshal(map[string]string{"url": server.URL})
	if err != nil {
		t.Fatal(err)
	}
	request := CreatePluginRequest{
		Name: "fetch_fixture", Source: httpPluginSource, TestSource: httpPluginTestSource,
		TestArguments: string(arguments), ExpectedOutput: "sunny, 21 C",
	}
	if err := env.CreatePlugin(t.Context(), agent.Name, request); err != nil {
		t.Fatal(err)
	}
	if err := env.UpdatePlugin(t.Context(), agent.Name, request); err != nil {
		t.Fatal(err)
	}
	result, err := agent.plugins[0].Binary.call(t.Context(), request.Name, arguments)
	if err != nil || result != request.ExpectedOutput {
		t.Fatalf("HTTP fixture call: %q %v", result, err)
	}
}

func TestPluginSampleErrorTextIsNotFailure(t *testing.T) {
	env, agent, request := setupPluginUpdateTest(t)
	request.TestArguments = `{"input":"ERROR is ordinary output"}`
	request.ExpectedOutput = "ERROR is ordinary output"
	if err := env.CreatePlugin(t.Context(), agent.Name, request); err != nil {
		t.Fatal(err)
	}
}

func TestPluginSuccessTestLimits(t *testing.T) {
	directory := t.TempDir()
	if err := buildPlugin(t.Context(), directory, examplePluginSource); err != nil {
		t.Fatal(err)
	}
	t.Run("output", func(t *testing.T) {
		source := `package main; import ("fmt"; "strings"; "testing"); func TestToolSuccess(t *testing.T) { fmt.Print(strings.Repeat("x", 2<<20)) }`
		if err := testPlugin(t.Context(), directory, source); err == nil || !strings.Contains(err.Error(), "output exceeds") {
			t.Fatalf("unbounded test output: %v", err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		source := `package main; import ("testing"; "time"); func TestToolSuccess(t *testing.T) { time.Sleep(time.Minute) }`
		if err := testPlugin(ctx, directory, source); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("unbounded test duration: %v", err)
		}
	})
}

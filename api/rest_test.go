package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func startTestRuntime(t *testing.T, root string) (*http.Client, func()) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "runtime.sock")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var serveErr error
	go func() {
		defer close(done)
		serveErr = ServeRuntime(ctx, socket, root)
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
			if serveErr != nil {
				t.Errorf("runtime failed: %v", serveErr)
			}
		case <-time.After(10 * time.Second):
			t.Error("runtime did not shut down")
		}
	}
	t.Cleanup(stop)
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: time.Second}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		response, err := client.Get("http://localhost/environments")
		if err == nil {
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("readiness returned %s", response.Status)
			}
			return client, stop
		}
		select {
		case <-done:
			t.Fatalf("runtime stopped before readiness: %v", serveErr)
		case <-deadline.C:
			t.Fatalf("runtime not ready: %v", err)
		case <-ticker.C:
		}
	}
}

func TestRuntimeRestoresEnvironments(t *testing.T) {
	ctx, root, _ := setupPersistenceTest(t)
	registry := BuiltinTools()
	cli, err := NewAgentEnvironment(ctx, root, "cli-environment")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.CreateAgent(ctx, "test-model", []Tool{registry["get_time"]}, "cli-agent", "", true); err != nil {
		t.Fatal(err)
	}
	client, stop := startTestRuntime(t, root)
	post := func(path string, wantStatus int) {
		t.Helper()
		response, err := client.Post("http://localhost"+path, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != wantStatus {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("POST %s: got %s (%s), want %d", path, response.Status, body, wantStatus)
		}
	}
	post("/environments/test", http.StatusCreated)
	post("/environments/test/agents/assistant?model=test-model", http.StatusCreated)
	stop()
	loaded, err := LoadAgentEnvironment(ctx, root, "test", registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Agents) != 1 || loaded.InitialAgent != loaded.Agents[0] {
		t.Fatal("REST-created agent was not persisted")
	}
	client, stop = startTestRuntime(t, root)
	response, err := client.Get("http://localhost/environments")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var names []string
	if err := json.NewDecoder(response.Body).Decode(&names); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"cli-environment", "test"}) {
		t.Fatalf("environments not restored: %v", names)
	}
	post("/environments/test", http.StatusConflict)
	post("/environments/test/agents/assistant?model=test-model", http.StatusConflict)
	post("/environments/test/agents/second?model=test-model", http.StatusCreated)
	stop()
	restored, err := LoadAgentEnvironment(ctx, root, "test", registry)
	if err != nil {
		t.Fatal(err)
	}
	if restored.ID != loaded.ID || len(restored.Agents) != 2 || restored.InitialAgent.Session.SessionID != loaded.InitialAgent.Session.SessionID {
		t.Fatal("restart lost original identity, agents, or session")
	}
}

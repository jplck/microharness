package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var examplePluginSource string

const examplePluginTestSource = `package main

import (
	"encoding/json"
	"testing"
)

func TestToolSuccess(t *testing.T) {
	result, err := echoTool().Call(json.RawMessage("{\"input\":\"hello\"}"))
	if err != nil || result != "hello" {
		t.Fatalf("echo success: %q %v", result, err)
	}
}
`

func TestMain(tests *testing.M) {
	source, err := os.ReadFile("../examples/echo/main.go")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	examplePluginSource = string(source)
	os.Exit(tests.Run())
}

func TestNativeToolRegistry(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	registry := DefaultTools()
	if len(registry) != 12 {
		t.Fatalf("tool count: %d", len(registry))
	}
	for name, tool := range registry {
		if tool.bind == nil {
			t.Fatalf("built-in %s is not native", name)
		}
	}
	tool := registry["get_time"].bind(ToolContext{Environment: &AgentEnvironment{}, Caller: "caller"})
	result, err := tool.Execute(t.Context(), json.RawMessage(`{"location":"UTC"}`))
	if err != nil || !strings.HasPrefix(result, "Current time in UTC: ") {
		t.Fatalf("native call: %q %v", result, err)
	}
}

func TestCreatePlugin(t *testing.T) {
	ctx, root, _ := setupPersistenceTest(t)
	env, err := NewAgentEnvironment(ctx, root, "plugins")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := env.CreateAgent(ctx, "test-model", []Tool{DefaultTools()["create_tool"]}, "maker", "", true)
	if err != nil {
		t.Fatal(err)
	}
	request := CreatePluginRequest{Name: "echo", Source: examplePluginSource, TestSource: examplePluginTestSource, TestArguments: `{"input":"hello"}`, ExpectedOutput: "hello"}
	arguments, _ := json.Marshal(request)
	agent.Client = messageTestModel{call: func(ctx context.Context, messages []Message) (Message, error) {
		if messages[len(messages)-1].Role == "user" {
			return Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "create", Name: "create_tool", Arguments: arguments}, {ID: "invoke", Name: "echo", Arguments: json.RawMessage(`{"input":"created"}`)}}}, nil
		}
		if got := messages[len(messages)-1].Content; got != "created" {
			t.Fatalf("same-turn invocation failed: %+v", messages)
		}
		return Message{Role: "assistant", Content: "done"}, nil
	}}
	if err := agent.executeEnvelope(ctx, Envelope{ID: "request", Source: "cli", To: "maker", Content: "create a tool"}); err != nil {
		t.Fatal(err)
	}
	if len(agent.plugins) != 1 || !strings.HasPrefix(agent.plugins[0].Path, "maker-echo-") ||
		!strings.HasSuffix(agent.plugins[0].Path, "/bin/echo") || filepath.Base(agent.plugins[0].Binary.Path) != "echo" {
		t.Fatalf("tool name missing from saved plugin path: %+v", agent.plugins)
	}
	info, err := os.Stat(agent.plugins[0].Binary.Path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		t.Fatal("named plugin executable missing:", err)
	}
	if err := env.CreatePlugin(ctx, "maker", request); !errors.Is(err, ErrInvalidTool) {
		t.Fatalf("duplicate allowed: %v", err)
	}
	for name := range DefaultTools() {
		reserved := request
		reserved.Name = name
		if err := env.CreatePlugin(ctx, "maker", reserved); !errors.Is(err, ErrInvalidTool) {
			t.Fatalf("private tool replaced native %s: %v", name, err)
		}
	}
	restored, err := LoadAgentEnvironment(ctx, root, "plugins", DefaultTools())
	if err != nil {
		t.Fatal(err)
	}
	loaded := restored.InitialAgent
	result, err := loaded.plugins[0].Binary.call(ctx, "echo", json.RawMessage(`{"input":"restored"}`))
	if err != nil || result != "restored" {
		t.Fatalf("restored named executable failed: %q %v", result, err)
	}
	if err := restored.UpdateAgent(ctx, "maker", "test-model", "updated", []Tool{DefaultTools()["create_tool"]}); err != nil {
		t.Fatal(err)
	}
	if len(loaded.currentTools()) != 11 {
		t.Fatal("private tool not restored")
	}
	if err := restored.SetAgentTools(ctx, "maker", nil); err != nil {
		t.Fatal(err)
	}
	if len(loaded.currentTools()) != 8 || len(loaded.plugins) != 1 {
		t.Fatal("disabling creation should retain but disable private tools")
	}
	if err := restored.CreatePlugin(ctx, "maker", request); !errors.Is(err, ErrInvalidTool) {
		t.Fatalf("revoked permission: %v", err)
	}
	directory, _, err := privatePluginPaths(restored.DataRoot, loaded.plugins[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.DeleteAgent(ctx, "maker", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleting agent did not erase the named plugin directory:", err)
	}
}

func TestPrivatePluginPaths(t *testing.T) {
	root := t.TempDir()
	for _, saved := range []string{
		"", ".", "..", "../escape", "/absolute", "dir/../echo", "dir/bin/../echo",
		"dir/bin/", "dir/bin/UPPER", "dir/bin/echo/extra", "dir/echo", "dir/other/echo",
		`dir\bin\echo`, "maker-old-id",
	} {
		if _, _, err := privatePluginPaths(root, saved); err == nil {
			t.Errorf("invalid saved path accepted: %q", saved)
		}
	}
	directory, executable, err := privatePluginPaths(root, "maker-echo-id/bin/echo")
	if err != nil || executable != filepath.Join(root, "plugins", "maker-echo-id", "bin", "echo") ||
		directory != filepath.Join(root, "plugins", "maker-echo-id") {
		t.Fatalf("resolve named plugin: %q %q %v", directory, executable, err)
	}
}

func TestPrivatePluginNamesAcrossAgents(t *testing.T) {
	ctx, root, _ := setupPersistenceTest(t)
	env, err := NewAgentEnvironment(ctx, root, "names")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, owner := range []string{"first", "second"} {
		agent, err := env.CreateAgent(ctx, "test-model", []Tool{DefaultTools()["create_tool"]}, owner, "", false)
		if err != nil {
			t.Fatal(err)
		}
		request := CreatePluginRequest{
			Name: "toolplugin", Source: strings.Replace(examplePluginSource, `Name: "echo"`, `Name: "toolplugin"`, 1),
			TestSource:    examplePluginTestSource,
			TestArguments: `{"input":"hello"}`, ExpectedOutput: "hello",
		}
		if err := env.CreatePlugin(ctx, owner, request); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, agent.plugins[0].Binary.Path)
		if filepath.Base(paths[len(paths)-1]) != "toolplugin" {
			t.Fatal("tool name collides with SDK directory")
		}
	}
	if paths[0] == paths[1] {
		t.Fatal("agents share a private executable")
	}
	for _, agent := range env.Agents {
		result, err := agent.plugins[0].Binary.call(ctx, "toolplugin", json.RawMessage(`{"input":"hello"}`))
		if err != nil || result != "hello" {
			t.Fatalf("private executable failed: %q %v", result, err)
		}
	}
}

func TestPluginCreationFailures(t *testing.T) {
	ctx, root, _ := setupPersistenceTest(t)
	env, err := NewAgentEnvironment(ctx, root, "failures")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := env.CreateAgent(ctx, "test-model", []Tool{DefaultTools()["create_tool"]}, "maker", "", false)
	if err != nil {
		t.Fatal(err)
	}
	request := CreatePluginRequest{Name: "echo", Source: examplePluginSource, TestSource: examplePluginTestSource, TestArguments: `{"input":"hello"}`, ExpectedOutput: "wrong"}
	if err := env.CreatePlugin(ctx, "maker", request); err == nil {
		t.Fatal("failed sample accepted")
	}
	if len(agent.plugins) != 0 {
		t.Fatal("failed sample registered")
	}
	request.ExpectedOutput = "hello"
	state := filepath.Join(env.DataRoot, "environment.json")
	if err := os.Rename(state, state+".backup"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := env.CreatePlugin(ctx, "maker", request); err == nil {
		t.Fatal("save failure ignored")
	}
	if len(agent.plugins) != 0 {
		t.Fatal("save failure did not roll back")
	}
	entries, err := os.ReadDir(filepath.Join(env.DataRoot, "plugins"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed build artifacts remain: %v %v", entries, err)
	}
}

func TestPluginProcessLimits(t *testing.T) {
	directory := t.TempDir()
	source := strings.Replace(examplePluginSource, `"fmt"`, "\"fmt\"\n\"strings\"", 1)
	source = strings.Replace(source, "return arguments.Input, nil", `switch arguments.Input { case "overflow": return strings.Repeat("x", 2<<20), nil; case "hang": for {}; case "invalid": fmt.Print("not json") }; return arguments.Input, nil`, 1)
	if err := buildPlugin(t.Context(), directory, source); err != nil {
		t.Fatal(err)
	}
	binary, err := readPlugin(t.Context(), filepath.Join(directory, "tool"))
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"overflow", "invalid"} {
		arguments, _ := json.Marshal(map[string]string{"input": input})
		if _, err := binary.call(t.Context(), "echo", arguments); err == nil {
			t.Fatalf("%s accepted", input)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := binary.call(ctx, "echo", json.RawMessage(`{"input":"hang"}`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
}

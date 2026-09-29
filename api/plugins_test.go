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

func TestMain(tests *testing.M) {
	source, err := os.ReadFile("../examples/echo/main.go")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	examplePluginSource = string(source)
	os.Exit(tests.Run())
}

func TestPluginCatalogue(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, directory := range []string{t.TempDir(), filepath.Join(t.TempDir(), "missing")} {
		registry, cache, err := loadPluginCatalogue(t.Context(), directory)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(cache) })
		if len(registry) != 9 {
			t.Fatalf("tool count: %d", len(registry))
		}
		for name, tool := range registry {
			if tool.plugin != nil || tool.bind == nil {
				t.Fatalf("built-in %s is not native", name)
			}
		}
		tool := registry["get_time"].bind(ToolContext{Environment: &AgentEnvironment{}, Caller: "caller"})
		result, err := tool.Execute(t.Context(), json.RawMessage(`{"location":"UTC"}`))
		if err != nil || !strings.HasPrefix(result, "Current time in UTC: ") {
			t.Fatalf("native call: %q %v", result, err)
		}
	}
}

func TestPluginCannotReplaceBuiltins(t *testing.T) {
	for _, name := range []string{"get_time", "message"} {
		t.Run(name, func(t *testing.T) {
			buildDirectory, pluginDirectory := t.TempDir(), t.TempDir()
			source := strings.Replace(examplePluginSource, `Name: "echo"`, `Name: "`+name+`"`, 1)
			if err := buildPlugin(t.Context(), buildDirectory, source); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(buildDirectory, "tool"), filepath.Join(pluginDirectory, "collision")); err != nil {
				t.Fatal(err)
			}
			_, cache, err := loadPluginCatalogue(t.Context(), pluginDirectory)
			if cache != "" {
				t.Cleanup(func() { os.RemoveAll(cache) })
			}
			if !errors.Is(err, ErrInvalidTool) {
				t.Fatalf("plugin replaced native %s: %v", name, err)
			}
		})
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
	request := CreatePluginRequest{Name: "echo", Source: examplePluginSource, TestArguments: `{"input":"hello"}`, ExpectedOutput: "hello"}
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
	if err := restored.UpdateAgent(ctx, "maker", "test-model", "updated", []Tool{DefaultTools()["create_tool"]}); err != nil {
		t.Fatal(err)
	}
	if len(loaded.currentTools()) != 8 {
		t.Fatal("private tool not restored")
	}
	if err := restored.SetAgentTools(ctx, "maker", nil); err != nil {
		t.Fatal(err)
	}
	if len(loaded.currentTools()) != 6 || len(loaded.plugins) != 1 {
		t.Fatal("disabling creation should retain but disable private tools")
	}
	if err := restored.CreatePlugin(ctx, "maker", request); !errors.Is(err, ErrInvalidTool) {
		t.Fatalf("revoked permission: %v", err)
	}
}

func TestPluginReplacement(t *testing.T) {
	ctx, root, _ := setupPersistenceTest(t)
	pluginDirectory, buildDirectory := t.TempDir(), t.TempDir()
	install := func(source string) {
		t.Helper()
		if err := buildPlugin(ctx, buildDirectory, source); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(buildDirectory, "tool"), filepath.Join(pluginDirectory, "echo")); err != nil {
			t.Fatal(err)
		}
	}
	load := func() ToolRegistry {
		t.Helper()
		registry, cache, err := loadPluginCatalogue(ctx, pluginDirectory)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(cache) })
		return registry
	}
	install(examplePluginSource)
	first := load()
	env, err := NewAgentEnvironment(ctx, root, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	env.registry = first
	agent, err := env.CreateAgent(ctx, "test-model", []Tool{first["echo"]}, "user", "", false)
	if err != nil {
		t.Fatal(err)
	}
	var old Tool
	for _, tool := range agent.currentTools() {
		if tool.Name == "echo" {
			old = tool
		}
	}
	install(strings.Replace(examplePluginSource, "return arguments.Input, nil", `return arguments.Input + "!", nil`, 1))
	env.mu.Lock()
	env.registry = load()
	env.mu.Unlock()
	for _, tool := range agent.currentTools() {
		if tool.Name == "echo" {
			result, err := tool.Execute(ctx, json.RawMessage(`{"input":"new"}`))
			if err != nil || result != "new!" {
				t.Fatalf("replacement: %q %v", result, err)
			}
		}
	}
	result, err := old.Execute(ctx, json.RawMessage(`{"input":"old"}`))
	if err != nil || result != "old" {
		t.Fatalf("old snapshot changed: %q %v", result, err)
	}
	if err := os.Remove(filepath.Join(pluginDirectory, "echo")); err != nil {
		t.Fatal(err)
	}
	env.mu.Lock()
	env.registry = load()
	env.mu.Unlock()
	for _, tool := range agent.currentTools() {
		if tool.Name == "echo" {
			t.Fatal("removed plugin still offered")
		}
		if tool.plugin != nil || !tool.Automatic {
			t.Fatalf("reload changed native automatic tool %s", tool.Name)
		}
	}
	if len(agent.currentTools()) != 6 {
		t.Fatal("reload removed native automatic tools")
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
	request := CreatePluginRequest{Name: "echo", Source: examplePluginSource, TestArguments: `{"input":"hello"}`, ExpectedOutput: "wrong"}
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

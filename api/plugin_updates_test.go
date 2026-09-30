package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func setupPluginUpdateTest(t *testing.T) (*AgentEnvironment, *Agent, CreatePluginRequest) {
	t.Helper()
	ctx, root, _ := setupPersistenceTest(t)
	env, err := NewAgentEnvironment(ctx, root, "updates")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := env.CreateAgent(ctx, "test-model", []Tool{DefaultTools()["create_tool"]}, "maker", "", true)
	if err != nil {
		t.Fatal(err)
	}
	request := CreatePluginRequest{
		Name: "echo", Source: examplePluginSource, TestSource: examplePluginTestSource,
		TestArguments: `{"input":"hello"}`, ExpectedOutput: "hello",
	}
	return env, agent, request
}

func updatedEchoRequest(request CreatePluginRequest) CreatePluginRequest {
	request.Source = strings.Replace(request.Source, "return arguments.Input, nil", `return "updated:" + arguments.Input, nil`, 1)
	request.TestSource = strings.Replace(request.TestSource, `result != "hello"`, `result != "updated:hello"`, 1)
	request.ExpectedOutput = "updated:hello"
	return request
}

func assertPluginResult(t *testing.T, tool Tool, expected string) {
	t.Helper()
	result, err := tool.Execute(t.Context(), json.RawMessage(`{"input":"hello"}`))
	if err != nil || result != expected {
		t.Fatalf("plugin call: got %q, %v; expected %q", result, err, expected)
	}
}

func TestUpdatePlugin(t *testing.T) {
	env, agent, request := setupPluginUpdateTest(t)
	ctx := t.Context()
	if err := env.CreatePlugin(ctx, agent.Name, request); err != nil {
		t.Fatal(err)
	}
	other, err := env.CreateAgent(ctx, "test-model", []Tool{DefaultTools()["create_tool"]}, "other", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.CreatePlugin(ctx, other.Name, request); err != nil {
		t.Fatal(err)
	}
	previous := agent.plugins[0]
	held := binaryTool(previous.Binary, previous.Binary.Definitions[0])
	previousNames := append([]string(nil), agent.ToolNames...)
	if err := env.UpdatePlugin(ctx, agent.Name, updatedEchoRequest(request)); err != nil {
		t.Fatal(err)
	}
	if len(agent.plugins) != 1 || !reflect.DeepEqual(agent.ToolNames, previousNames) {
		t.Fatal("update changed tool grants or added another private tool")
	}
	current := agent.plugins[0]
	if !reflect.DeepEqual(current.PreviousPaths, []string{previous.Path}) || len(previous.PreviousPaths) != 0 {
		t.Fatal("update did not preserve immutable version history")
	}
	if current.Path == previous.Path || current.Binary == previous.Binary ||
		current.Binary.Path == previous.Binary.Path || !strings.HasPrefix(current.Path, "maker-echo-") ||
		!strings.HasSuffix(current.Path, "/bin/echo") {
		t.Fatalf("update did not install an immutable named version: %+v", current)
	}
	assertPluginResult(t, held, "hello")
	assertPluginResult(t, binaryTool(current.Binary, current.Binary.Definitions[0]), "updated:hello")
	assertPluginResult(t, binaryTool(other.plugins[0].Binary, other.plugins[0].Binary.Definitions[0]), "hello")
	intermediate := current
	if err := env.UpdatePlugin(ctx, agent.Name, updatedEchoRequest(request)); err != nil {
		t.Fatal(err)
	}
	current = agent.plugins[0]
	if !reflect.DeepEqual(current.PreviousPaths, []string{previous.Path, intermediate.Path}) ||
		!reflect.DeepEqual(intermediate.PreviousPaths, []string{previous.Path}) {
		t.Fatal("repeated update lost or mutated prior version history")
	}
	assertPluginResult(t, binaryTool(intermediate.Binary, intermediate.Binary.Definitions[0]), "updated:hello")
	restored, err := LoadAgentEnvironment(ctx, filepath.Dir(env.DataRoot), env.Name, DefaultTools())
	if err != nil {
		t.Fatal(err)
	}
	loaded := restored.InitialAgent
	if len(loaded.plugins) != 1 || loaded.plugins[0].Path != current.Path ||
		!reflect.DeepEqual(loaded.plugins[0].PreviousPaths, []string{previous.Path, intermediate.Path}) {
		t.Fatalf("updated version not persisted: %+v", loaded.plugins)
	}
	assertPluginResult(t, binaryTool(loaded.plugins[0].Binary, loaded.plugins[0].Binary.Definitions[0]), "updated:hello")
	loadedOther := restored.agentLocked(other.Name)
	assertPluginResult(t, binaryTool(loadedOther.plugins[0].Binary, loadedOther.plugins[0].Binary.Definitions[0]), "hello")
	assertPluginResult(t, held, "hello")
	assertPluginResult(t, binaryTool(intermediate.Binary, intermediate.Binary.Definitions[0]), "updated:hello")
	var directories []string
	for _, path := range []string{previous.Path, intermediate.Path, current.Path} {
		directory, _, err := privatePluginPaths(restored.DataRoot, path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(directory, "main_test.go")); err != nil {
			t.Fatalf("saved success test source missing: %v", err)
		}
		directories = append(directories, directory)
	}
	if err := restored.DeleteAgent(ctx, loaded.Name, true); err != nil {
		t.Fatal(err)
	}
	for _, directory := range directories {
		if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("erased agent left plugin version %s: %v", directory, err)
		}
	}
	assertPluginResult(t, binaryTool(loadedOther.plugins[0].Binary, loadedOther.plugins[0].Binary.Definitions[0]), "hello")
}

func TestUpdatePluginBuiltin(t *testing.T) {
	env, agent, request := setupPluginUpdateTest(t)
	ctx := t.Context()
	available := func() map[string]Tool {
		tools := make(map[string]Tool)
		for _, tool := range agent.currentTools() {
			tools[tool.Name] = tool
		}
		return tools
	}
	if !DefaultTools()["update_tool"].Automatic {
		t.Fatal("update_tool should not require a separate grant")
	}
	if err := env.SetAgentTools(ctx, agent.Name, nil); err != nil {
		t.Fatal(err)
	}
	if _, exists := available()["update_tool"]; exists {
		t.Fatal("update_tool visible without create_tool")
	}
	if err := env.SetAgentTools(ctx, agent.Name, []Tool{DefaultTools()["create_tool"]}); err != nil {
		t.Fatal(err)
	}
	update, exists := available()["update_tool"]
	if !exists {
		t.Fatal("update_tool missing with create_tool")
	}
	if err := env.CreatePlugin(ctx, agent.Name, request); err != nil {
		t.Fatal(err)
	}
	previous := agent.plugins[0]
	arguments, err := json.Marshal(updatedEchoRequest(request))
	if err != nil {
		t.Fatal(err)
	}
	result, err := update.Execute(ctx, arguments)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(result), &response); err != nil ||
		response.Name != request.Name || response.Status != "updated" {
		t.Fatalf("builtin update response: %q %v", result, err)
	}
	if len(agent.plugins) != 1 || agent.plugins[0].Path == previous.Path {
		t.Fatal("builtin update did not replace the same-name private tool")
	}
	assertPluginResult(t, available()["echo"], "updated:hello")
	assertPluginResult(t, binaryTool(previous.Binary, previous.Binary.Definitions[0]), "hello")
	if err := env.SetAgentTools(ctx, agent.Name, nil); err != nil {
		t.Fatal(err)
	}
	if _, exists := available()["update_tool"]; exists {
		t.Fatal("update_tool visible after create_tool revoked")
	}
	if _, exists := available()["echo"]; exists {
		t.Fatal("private tool visible after create_tool revoked")
	}
	if _, err := update.Execute(ctx, arguments); !errors.Is(err, ErrInvalidTool) {
		t.Fatalf("held builtin bypassed revoked permission: %v", err)
	}
}

func TestUpdatePluginOwnership(t *testing.T) {
	env, agent, request := setupPluginUpdateTest(t)
	ctx := t.Context()
	if err := env.UpdatePlugin(ctx, agent.Name, request); !errors.Is(err, ErrInvalidTool) {
		t.Fatalf("nonexistent private tool update: %v", err)
	}
	if err := env.CreatePlugin(ctx, agent.Name, request); err != nil {
		t.Fatal(err)
	}
	other, err := env.CreateAgent(ctx, "test-model", []Tool{DefaultTools()["create_tool"]}, "other", "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, caller := range []string{"missing", other.Name} {
		if err := env.UpdatePlugin(ctx, caller, request); !errors.Is(err, ErrInvalidTool) {
			t.Fatalf("caller %s updated unowned plugin: %v", caller, err)
		}
	}
	for name := range DefaultTools() {
		native := request
		native.Name = name
		if err := env.UpdatePlugin(ctx, agent.Name, native); !errors.Is(err, ErrInvalidTool) {
			t.Fatalf("native tool %s updated: %v", name, err)
		}
	}
	if err := env.SetAgentTools(ctx, agent.Name, nil); err != nil {
		t.Fatal(err)
	}
	if err := env.UpdatePlugin(ctx, agent.Name, request); !errors.Is(err, ErrInvalidTool) {
		t.Fatalf("update without create_tool grant: %v", err)
	}
}

func TestPluginSuccessTestsRequired(t *testing.T) {
	env, agent, request := setupPluginUpdateTest(t)
	if err := env.CreatePlugin(t.Context(), agent.Name, request); err != nil {
		t.Fatal(err)
	}
	original := agent.plugins[0]
	tests := []struct {
		name   string
		source string
	}{
		{"missing_source", ""},
		{"oversized_source", strings.Repeat(" ", (64<<10)+1)},
		{"external_package", "package main_test"},
		{"missing_test", "package main"},
		{"wrong_test", `package main; import "testing"; func TestInvalidInput(t *testing.T) {}`},
		{"failing_test", `package main; import "testing"; func TestToolSuccess(t *testing.T) { t.Fatal("failure") }`},
		{"skipped_test", `package main; import "testing"; func TestToolSuccess(t *testing.T) { t.Skip("not executed") }`},
		{"testmain_bypass", `package main; import ("os"; "testing"); func TestMain(m *testing.M) { os.Exit(0) }; func TestToolSuccess(t *testing.T) { t.Fatal("unreachable") }`},
		{"compile_error", `package main; import "testing"; func TestToolSuccess(t *testing.T) { missing() }`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invalid := request
			invalid.TestSource = test.source
			if err := env.UpdatePlugin(t.Context(), agent.Name, invalid); err == nil {
				t.Fatal("invalid update success test accepted")
			}
			invalid.Name = "other_echo"
			invalid.Source = strings.Replace(invalid.Source, `Name: "echo"`, `Name: "other_echo"`, 1)
			if err := env.CreatePlugin(t.Context(), agent.Name, invalid); err == nil {
				t.Fatal("invalid create success test accepted")
			}
			if len(agent.plugins) != 1 || !reflect.DeepEqual(agent.plugins[0], original) {
				t.Fatal("test failure changed installed plugin")
			}
		})
	}
	assertPluginResult(t, binaryTool(original.Binary, original.Binary.Definitions[0]), "hello")
	entries, err := os.ReadDir(filepath.Join(env.DataRoot, "plugins"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("failed validation artifacts remain: %v %v", entries, err)
	}
}

func TestUpdatePluginFailuresPreserveVersion(t *testing.T) {
	env, agent, request := setupPluginUpdateTest(t)
	if err := env.CreatePlugin(t.Context(), agent.Name, request); err != nil {
		t.Fatal(err)
	}
	original := agent.plugins[0]
	held := binaryTool(original.Binary, original.Binary.Definitions[0])
	for _, name := range []string{"build", "sample", "sample_error", "manifest", "persistence"} {
		t.Run(name, func(t *testing.T) {
			replacement := updatedEchoRequest(request)
			switch name {
			case "build":
				replacement.Source = "package main\nfunc main() { nonexistent() }"
			case "sample":
				replacement.ExpectedOutput = "wrong"
			case "sample_error":
				replacement.TestArguments = `{"input":123}`
			case "manifest":
				replacement.Source = strings.Replace(replacement.Source, `Name: "echo"`, `Name: "different"`, 1)
			case "persistence":
				state := filepath.Join(env.DataRoot, "environment.json")
				if err := os.Rename(state, state+".backup"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(state, 0700); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Remove(state); err != nil {
						t.Error(err)
					}
					if err := os.Rename(state+".backup", state); err != nil {
						t.Error(err)
					}
				})
			}
			if err := env.UpdatePlugin(t.Context(), agent.Name, replacement); err == nil {
				t.Fatal("failed replacement accepted")
			}
			if len(agent.plugins) != 1 || !reflect.DeepEqual(agent.plugins[0], original) {
				t.Fatal("failed replacement changed live version")
			}
			assertPluginResult(t, held, "hello")
			entries, err := os.ReadDir(filepath.Join(env.DataRoot, "plugins"))
			if err != nil || len(entries) != 1 {
				t.Fatalf("failed replacement artifacts remain: %v %v", entries, err)
			}
		})
	}
	restored, err := LoadAgentEnvironment(t.Context(), filepath.Dir(env.DataRoot), env.Name, DefaultTools())
	if err != nil {
		t.Fatal(err)
	}
	loaded := restored.InitialAgent.plugins[0]
	if loaded.Path != original.Path {
		t.Fatal("failed replacement changed persisted version")
	}
	assertPluginResult(t, binaryTool(loaded.Binary, loaded.Binary.Definitions[0]), "hello")
}

func TestUpdatePluginConflicts(t *testing.T) {
	for _, change := range []string{"replacement", "owner_deletion", "owner_recreation", "permission_revoked"} {
		t.Run(change, func(t *testing.T) {
			env, agent, request := setupPluginUpdateTest(t)
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			if err := env.CreatePlugin(ctx, agent.Name, request); err != nil {
				t.Fatal(err)
			}
			barrier := t.TempDir()
			ready, release := filepath.Join(barrier, "ready"), filepath.Join(barrier, "release")
			delayed := updatedEchoRequest(request)
			delayed.TestSource = strings.Replace(delayed.TestSource, `"testing"`, "\"testing\"\n\"os\"\n\"time\"", 1)
			delayed.TestSource = strings.Replace(delayed.TestSource, "func TestToolSuccess(t *testing.T) {", fmt.Sprintf(`func TestToolSuccess(t *testing.T) {
				if err := os.WriteFile(%q, []byte("ready"), 0600); err != nil { t.Fatal(err) }
				for { if _, err := os.Stat(%q); err == nil { break }; time.Sleep(10*time.Millisecond) }
			`, ready, release), 1)
			done := make(chan error, 1)
			go func() {
				done <- env.UpdatePlugin(ctx, agent.Name, delayed)
				close(done)
			}()
			t.Cleanup(func() {
				os.WriteFile(release, []byte("release"), 0600)
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
				}
			})
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
		waitReady:
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("update exited before barrier: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-ticker.C:
					continue waitReady
				}
			}
			expectedErr := ErrPluginUpdateConflict
			switch change {
			case "replacement":
				if err := env.UpdatePlugin(ctx, agent.Name, request); err != nil {
					t.Fatal(err)
				}
			case "owner_deletion", "owner_recreation":
				if err := env.DeleteAgent(ctx, agent.Name, false); err != nil {
					t.Fatal(err)
				}
				if change == "owner_recreation" {
					if _, err := env.CreateAgent(ctx, "test-model", []Tool{DefaultTools()["create_tool"]}, agent.Name, "", true); err != nil {
						t.Fatal(err)
					}
				}
			case "permission_revoked":
				if err := env.SetAgentTools(ctx, agent.Name, nil); err != nil {
					t.Fatal(err)
				}
				expectedErr = ErrInvalidTool
			}
			if err := os.WriteFile(release, []byte("release"), 0600); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, expectedErr) {
					t.Fatalf("stale update: %v, expected %v", err, expectedErr)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			for _, owner := range env.Agents {
				for _, plugin := range owner.plugins {
					assertPluginResult(t, binaryTool(plugin.Binary, plugin.Binary.Definitions[0]), "hello")
				}
			}
			entries, err := os.ReadDir(filepath.Join(env.DataRoot, "plugins"))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".build-") {
					t.Fatal("stale update build remains")
				}
			}
		})
	}
}

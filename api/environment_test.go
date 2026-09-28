package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func setupPersistenceTest(t *testing.T) (context.Context, string, ToolRegistry) {
	t.Helper()
	t.Chdir(t.TempDir())
	t.Setenv("MICRO_TEST_KEY", "secret-test-value")
	if err := os.WriteFile("models.json", []byte(`{"models":[{"name":"test-model","provider":"openai","endpoint":"http://localhost","apiKeyEnv":"MICRO_TEST_KEY"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := ToolRegistry{"echo": {
		Name: "echo",
		Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
			return string(raw), nil
		},
	}}
	return t.Context(), t.TempDir(), registry
}

func TestEnvironmentRoundTrip(t *testing.T) {
	ctx, root, registry := setupPersistenceTest(t)
	env, err := NewAgentEnvironment(ctx, root, "test")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := env.CreateAgent(ctx, "test-model", []Tool{registry["echo"]}, "assistant", "Be concise.", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.CreateAgent(ctx, "test-model", nil, "other", "", true); err != nil {
		t.Fatal(err)
	}
	if err := agent.Session.AddMessage(Message{Role: "user", Content: "Remember this conversation"}); err != nil {
		t.Fatal(err)
	}
	agent.Inbox = []Envelope{{ID: "message-1", Content: "pending"}}
	env.MemoryStore.EmbeddingModel = ""
	if err := env.MemoryStore.Add(ctx, Memory{Kind: "fact", Content: "Saved memory"}); err != nil {
		t.Fatal(err)
	}
	if err := env.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadAgentEnvironment(ctx, root, "test", registry)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != env.ID || len(loaded.Agents) != 2 || loaded.InitialAgent != loaded.Agents[1] {
		t.Fatal("environment identity or initial agent not restored")
	}
	restored := loaded.Agents[0]
	if restored.Client == nil || restored.ModelName != "test-model" || restored.Instructions != "Be concise." || restored.Session.SessionID != agent.Session.SessionID {
		t.Fatal("agent configuration not restored")
	}
	if !reflect.DeepEqual(restored.Session.Messages, agent.Session.Messages) || !reflect.DeepEqual(restored.Inbox, agent.Inbox) {
		t.Fatal("messages or inbox not restored")
	}
	wantMemories, err := json.Marshal(env.MemoryStore.entries)
	if err != nil {
		t.Fatal(err)
	}
	gotMemories, err := json.Marshal(loaded.MemoryStore.entries)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotMemories) != string(wantMemories) {
		t.Fatal("shared memories not restored")
	}
	if len(restored.Tools) != len(MemoryTools(ctx, loaded.MemoryStore))+len(loaded.messagingTools(restored.Name))+1 {
		t.Fatal("tool set not restored")
	}
	for _, tool := range restored.Tools {
		if tool.Execute == nil {
			t.Fatalf("tool %q has no handler", tool.Name)
		}
		if tool.Name == "echo" {
			output, err := tool.Execute(ctx, json.RawMessage(`"hello"`))
			if err != nil || output != `"hello"` {
				t.Fatalf("restored tool failed: %q, %v", output, err)
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(env.DataRoot, "environment.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret-test-value", root, "Remember this conversation", "Execute"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("snapshot contains runtime or duplicated state %q", forbidden)
		}
	}
	if _, err := NewAgentEnvironment(ctx, root, "test"); err == nil {
		t.Fatal("existing environment overwritten")
	}
	for _, tool := range restored.Tools {
		if tool.Name == "write_memory" {
			if _, err := tool.Execute(ctx, json.RawMessage(`{"kind":"fact","content":"Restored tool memory"}`)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(loaded.MemoryStore.entries) != 2 || len(env.MemoryStore.entries) != 1 {
		t.Fatal("memory tools were not bound to the restored store")
	}
}

func TestEnvironmentRejectsInvalidState(t *testing.T) {
	ctx, root, registry := setupPersistenceTest(t)
	env, err := NewAgentEnvironment(ctx, root, "test")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := env.CreateAgent(ctx, "test-model", []Tool{registry["echo"]}, "assistant", "", true)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(env.DataRoot, "environment.json")
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*EnvironmentState)
		want   string
	}{
		{"version", func(state *EnvironmentState) { state.Version = 99 }, "unsupported environment version"},
		{"identity", func(state *EnvironmentState) { state.Name = "different" }, "invalid environment identity"},
		{"initial agent", func(state *EnvironmentState) { state.InitialAgentName = "missing" }, "initial agent"},
		{"duplicate agent", func(state *EnvironmentState) { state.Agents = append(state.Agents, state.Agents[0]) }, "duplicate agent"},
		{"missing tool", func(state *EnvironmentState) { state.Agents[0].ToolNames = []string{"missing"} }, "requires registered tool"},
		{"duplicate tool", func(state *EnvironmentState) { state.Agents[0].ToolNames = []string{"echo", "echo"} }, "duplicate tool"},
		{"missing model", func(state *EnvironmentState) { state.Agents[0].ModelName = "missing" }, "model \"missing\" not found"},
		{"empty session", func(state *EnvironmentState) { state.Agents[0].SessionID = "" }, "invalid or duplicate agent"},
		{"session traversal", func(state *EnvironmentState) { state.Agents[0].SessionID = "../outside" }, "session ID"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var state EnvironmentState
			if err := json.Unmarshal(data, &state); err != nil {
				t.Fatal(err)
			}
			test.mutate(&state)
			if err := writeJSONAtomic(filename, state); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadAgentEnvironment(ctx, root, "test", registry); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want error containing %q", err, test.want)
			}
		})
	}
	if err := os.WriteFile(filename, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(agent.Session.Scope, agent.Session.SessionID+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAgentEnvironment(ctx, root, "test", registry); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing session must fail, got %v", err)
	}
	if _, err := loadAgentEnvironments(ctx, root, registry); err == nil {
		t.Fatal("startup silently ignored an unrestorable environment")
	}
	for _, name := range []string{"", "..", "../outside", "/absolute"} {
		if _, err := NewAgentEnvironment(ctx, root, name); err == nil {
			t.Fatalf("accepted invalid name %q", name)
		}
	}
}

func TestEnvironmentLegacyAndRelocation(t *testing.T) {
	ctx, root, registry := setupPersistenceTest(t)
	filename := filepath.Join(root, "legacy", "environment.json")
	legacy := map[string]any{
		"ID": "original-id", "Name": "legacy", "Agents": []any{},
		"MemoryStore": map[string]string{"Path": "/old/memory.json", "EmbeddingModel": "legacy-embedding"},
	}
	if err := writeJSONAtomic(filename, legacy); err != nil {
		t.Fatal(err)
	}
	env, err := LoadAgentEnvironment(ctx, root, "legacy", registry)
	if err != nil {
		t.Fatal(err)
	}
	if env.ID != "original-id" || env.MemoryStore.EmbeddingModel != "legacy-embedding" {
		t.Fatal("legacy metadata lost")
	}
	agent, err := env.CreateAgent(ctx, "test-model", nil, "assistant", "", true)
	if err != nil {
		t.Fatal(err)
	}
	legacySession := map[string]any{
		"SessionID": "wrong-id", "Scope": "/old/Sessions",
		"Messages": []Message{{Role: "system", Content: "Preserve the original system prompt"}},
	}
	if err := writeJSONAtomic(filepath.Join(agent.Session.Scope, agent.Session.SessionID+".json"), legacySession); err != nil {
		t.Fatal(err)
	}
	newRoot := t.TempDir()
	if err := os.Rename(env.DataRoot, filepath.Join(newRoot, "legacy")); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadAgentEnvironment(ctx, newRoot, "legacy", registry)
	if err != nil {
		t.Fatal(err)
	}
	session := &loaded.InitialAgent.Session
	if session.SessionID != agent.Session.SessionID || session.Scope != filepath.Join(newRoot, "legacy", "Sessions") {
		t.Fatal("legacy session overwrote its runtime identity or path")
	}
	if len(session.Messages) != 1 || session.Messages[0].Content != "Preserve the original system prompt" {
		t.Fatal("legacy messages were not preserved")
	}
	if err := session.AddMessage(Message{Role: "user", Content: "After relocation"}); err != nil {
		t.Fatal(err)
	}
	if loaded.MemoryStore.Path != filepath.Join(newRoot, "legacy", "memory.json") {
		t.Fatal("memory path was not rebased")
	}
}

func TestPersistenceWriteFailure(t *testing.T) {
	ctx, root, _ := setupPersistenceTest(t)
	env, err := NewAgentEnvironment(ctx, root, "test")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := env.CreateAgent(ctx, "test-model", nil, "first", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.CreateAgent(ctx, "test-model", nil, "first", "", true); err == nil {
		t.Fatal("accepted duplicate agent")
	}
	filename := filepath.Join(env.DataRoot, "environment.json")
	original, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filename, make(chan int)); err == nil {
		t.Fatal("expected encoding failure")
	}
	after, err := os.ReadFile(filename)
	if err != nil || string(after) != string(original) {
		t.Fatal("encoding failure damaged existing snapshot")
	}
	if err := os.Rename(filename, filename+".backup"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filename, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := env.CreateAgent(ctx, "test-model", nil, "second", "", true); err == nil {
		t.Fatal("expected save failure")
	}
	if len(env.Agents) != 1 || env.InitialAgent != agent {
		t.Fatal("failed creation changed the agent list or initial agent")
	}
	session := Session{SessionID: "test", Scope: filename + ".backup"}
	if err := session.AddMessage(Message{Role: "user", Content: "unsaved"}); err == nil || len(session.Messages) != 0 {
		t.Fatal("failed session write did not roll back appended message")
	}
}

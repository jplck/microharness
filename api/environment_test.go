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

func TestSetAgentTools(t *testing.T) {
	ctx, root, registry := setupPersistenceTest(t)
	env, err := NewAgentEnvironment(ctx, root, "test")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := env.CreateAgent(ctx, "test-model", nil, "assistant", "Keep instructions", false)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := agent.Session.SessionID
	if err := env.SetAgentTools(ctx, "assistant", []Tool{registry["echo"]}); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadAgentEnvironment(ctx, root, "test", registry)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Agents[0].ToolNames, []string{"echo"}) || loaded.Agents[0].Session.SessionID != sessionID || loaded.Agents[0].Instructions != "Keep instructions" {
		t.Fatal("tool edit lost configuration or did not persist")
	}
	for _, tools := range [][]Tool{{registry["echo"], registry["echo"]}, {{Name: "invalid"}}, {MemoryTools(ctx, env.MemoryStore)[0]}} {
		if err := env.SetAgentTools(ctx, "assistant", tools); !errors.Is(err, ErrInvalidTool) {
			t.Fatalf("invalid tools accepted: %v", err)
		}
	}
	agent.runMu.Lock()
	err = env.SetAgentTools(ctx, "assistant", nil)
	agent.runMu.Unlock()
	if !errors.Is(err, ErrAgentBusy) {
		t.Fatalf("active turn not protected: %v", err)
	}
	agent.Inbox = []Envelope{{ID: "pending"}}
	if err := env.SetAgentTools(ctx, "assistant", nil); !errors.Is(err, ErrAgentBusy) {
		t.Fatalf("pending work not protected: %v", err)
	}
	agent.Inbox = nil
	previousRoot := env.DataRoot
	env.DataRoot = filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(env.DataRoot, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = env.SetAgentTools(ctx, "assistant", nil)
	env.DataRoot = previousRoot
	if err == nil || !reflect.DeepEqual(agent.ToolNames, []string{"echo"}) {
		t.Fatal("failed save changed tool assignments")
	}
	if err := env.SetAgentTools(ctx, "assistant", nil); err != nil {
		t.Fatal(err)
	}
	if len(agent.ToolNames) != 0 || len(agent.Tools) != 5 {
		t.Fatal("clearing optional tools removed automatic tools")
	}
	if err := env.SetAgentTools(ctx, "missing", nil); !errors.Is(err, ErrAgentNotFound) {
		t.Fatal("missing agent accepted")
	}
}

func TestUpdateAgent(t *testing.T) {
	ctx, root, registry := setupPersistenceTest(t)
	if err := writeJSONAtomic("models.json", map[string]any{"models": []Model{{Name: "test-model", Provider: ProviderOpenAI}, {Name: "other-model", Provider: ProviderOpenAI}}}); err != nil {
		t.Fatal(err)
	}
	env, err := NewAgentEnvironment(ctx, root, "test")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := env.CreateAgent(ctx, "test-model", nil, "assistant", "Original", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Session.AddMessage(Message{Role: "assistant", Content: "History"}); err != nil {
		t.Fatal(err)
	}
	sessionID := agent.Session.SessionID
	if err := env.UpdateAgent(ctx, "assistant", "other-model", "Updated", []Tool{registry["echo"]}); err != nil {
		t.Fatal(err)
	}
	if agent.ModelName != "other-model" || agent.Instructions != "Updated" || agent.Session.SessionID != sessionID || agent.Session.Messages[1].Content != "History" || agent.Session.Messages[0].Content != agentInstructions("Updated") || env.InitialAgent != agent {
		t.Fatal("edit did not preserve identity/history or update settings")
	}
	loaded, err := LoadAgentEnvironment(ctx, root, "test", registry)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.InitialAgent.ModelName != "other-model" || !reflect.DeepEqual(loaded.InitialAgent.Session.Messages, agent.Session.Messages) || !reflect.DeepEqual(loaded.InitialAgent.ToolNames, []string{"echo"}) {
		t.Fatal("updated agent did not restore")
	}
	if err := env.UpdateAgent(ctx, "assistant", "missing", "bad", nil); !errors.Is(err, ErrModelNotFound) {
		t.Fatal("unknown model accepted")
	}
	if err := env.UpdateAgent(ctx, "assistant", "test-model", "bad", []Tool{{Name: "bad"}}); !errors.Is(err, ErrInvalidTool) {
		t.Fatal("invalid tools accepted")
	}
	agent.runMu.Lock()
	err = env.UpdateAgent(ctx, "assistant", "test-model", "busy", nil)
	agent.runMu.Unlock()
	if !errors.Is(err, ErrAgentBusy) {
		t.Fatal("active turn edited")
	}
	agent.Inbox = []Envelope{{ID: "pending"}}
	err = env.UpdateAgent(ctx, "assistant", "test-model", "busy", nil)
	agent.Inbox = nil
	if !errors.Is(err, ErrAgentBusy) {
		t.Fatal("queued turn edited")
	}
	previousRoot := env.DataRoot
	env.DataRoot = filepath.Join(root, "blocked")
	if err := os.WriteFile(env.DataRoot, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = env.UpdateAgent(ctx, "assistant", "test-model", "failed", nil)
	env.DataRoot = previousRoot
	if err == nil || agent.ModelName != "other-model" || agent.Instructions != "Updated" || agent.Session.Messages[0].Content != agentInstructions("Updated") {
		t.Fatal("failed save changed live settings")
	}
	saved := Session{SessionID: sessionID, Scope: agent.Session.Scope}
	if err := saved.Load(); err != nil {
		t.Fatal(err)
	}
	if saved.Messages[0].Content != agentInstructions("Updated") {
		t.Fatal("failed edit did not restore session prompt")
	}
	setSessionInstructions(&saved, "Interrupted edit")
	if err := saved.persist(); err != nil {
		t.Fatal(err)
	}
	loaded, err = LoadAgentEnvironment(ctx, root, "test", registry)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.InitialAgent.Session.Messages[0].Content != agentInstructions("Updated") {
		t.Fatal("restore did not reconcile interrupted edit with committed settings")
	}
	if err := env.UpdateAgent(ctx, "assistant", "test-model", "", nil); err != nil {
		t.Fatal(err)
	}
	if len(agent.ToolNames) != 0 || agent.Session.Messages[0].Content != agentInstructions("") {
		t.Fatal("clearing instructions and tools failed")
	}
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
	if err := env.saveLocked(); err != nil {
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
	for _, tool := range restored.Tools {
		if tool.Name == "update_memory" {
			arguments, _ := json.Marshal(map[string]string{"id": loaded.MemoryStore.entries[1].ID, "kind": "fact", "content": "Updated tool memory"})
			if _, err := tool.Execute(ctx, arguments); err != nil {
				t.Fatal(err)
			}
		}
	}
	if updated := loaded.MemoryStore.entries[1]; updated.Content != "Updated tool memory" || updated.Revision != 2 {
		t.Fatalf("update_memory did not apply: %+v", updated)
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
		{"missing model", func(state *EnvironmentState) { state.Agents[0].ModelName = "missing" }, "model not found: \"missing\""},
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

func TestEnvironmentRelocation(t *testing.T) {
	ctx, root, registry := setupPersistenceTest(t)
	env, err := NewAgentEnvironment(ctx, root, "moved")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := env.CreateAgent(ctx, "test-model", nil, "assistant", "", true)
	if err != nil {
		t.Fatal(err)
	}
	newRoot := t.TempDir()
	if err := os.Rename(env.DataRoot, filepath.Join(newRoot, "moved")); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadAgentEnvironment(ctx, newRoot, "moved", registry)
	if err != nil {
		t.Fatal(err)
	}
	session := &loaded.InitialAgent.Session
	if session.SessionID != agent.Session.SessionID || session.Scope != filepath.Join(newRoot, "moved", "Sessions") {
		t.Fatal("session identity or path was not rebased")
	}
	if len(session.Messages) != 1 || session.Messages[0].Role != "system" {
		t.Fatal("session messages were not preserved")
	}
	if err := session.AddMessage(Message{Role: "user", Content: "After relocation"}); err != nil {
		t.Fatal(err)
	}
	if loaded.MemoryStore.Path != filepath.Join(newRoot, "moved", "memory.json") {
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

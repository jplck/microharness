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

func TestToolCatalogueBinding(t *testing.T) {
	ctx, root, _ := setupPersistenceTest(t)
	registry := DefaultTools()
	optional := registry.Optional()
	if len(optional) != 2 || optional[0].Name != "create_agent" || optional[1].Name != "get_time" {
		t.Fatalf("unexpected optional tools: %+v", optional)
	}
	for _, name := range []string{"search_memory", "write_memory", "update_memory", "message", "list_agents", "agent_status"} {
		if !registry[name].Automatic {
			t.Fatalf("%s not automatic", name)
		}
		if _, err := registry.Resolve([]string{name}); !errors.Is(err, ErrInvalidTool) {
			t.Fatalf("automatic tool selectable: %s %v", name, err)
		}
	}
	if _, err := registry.Resolve([]string{"missing"}); !errors.Is(err, ErrInvalidTool) {
		t.Fatal("unknown tool resolved")
	}
	if _, _, err := registry.Bind(ToolContext{}, nil); !errors.Is(err, ErrInvalidTool) {
		t.Fatal("missing caller context accepted")
	}
	first, err := NewAgentEnvironment(ctx, root, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewAgentEnvironment(ctx, root, "second")
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range []*AgentEnvironment{first, second} {
		for _, name := range []string{"caller", "recipient"} {
			if _, err := env.CreateAgent(ctx, "test-model", nil, name, "", false); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, env := range []*AgentEnvironment{first, second} {
		tools, names, err := registry.Bind(ToolContext{Environment: env, Caller: "caller"}, nil)
		if err != nil || len(tools) != 6 || len(names) != 0 {
			t.Fatalf("automatic binding: %v %v", names, err)
		}
		for _, tool := range tools {
			if !tool.Automatic {
				t.Fatalf("bound tool lost automatic classification: %s", tool.Name)
			}
			if registry[tool.Name].Execute != nil {
				t.Fatal("binding mutated shared definition")
			}
			if tool.Name == "message" {
				callCtx := context.WithValue(ctx, envelopeContextKey{}, Envelope{ConversationID: env.Name})
				if _, err := tool.Execute(callCtx, json.RawMessage(`{"to":"recipient","content":"work","sender":"spoofed","conversation_id":"spoofed"}`)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, env := range []*AgentEnvironment{first, second} {
		inbox, err := env.Inbox("recipient")
		if err != nil || len(inbox.Messages) != 1 || inbox.Messages[0].Sender != "caller" || inbox.Messages[0].ConversationID != env.Name {
			t.Fatalf("caller/environment isolation failed: %+v %v", inbox, err)
		}
	}
	if _, err := first.CreateChildAgent(ctx, "caller", ChildAgentRequest{Name: "denied", Model: "test-model"}); !errors.Is(err, ErrInvalidTool) {
		t.Fatalf("domain operation bypassed creation permission: %v", err)
	}
}

func TestCreateAgentTool(t *testing.T) {
	ctx, root, registry := setupPersistenceTest(t)
	registry["create_agent"] = CreateAgentTool()
	env, err := NewAgentEnvironment(ctx, root, "team")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := env.CreateAgentWithModels(ctx, "test-model", []Tool{registry["create_agent"]}, "parent", "Delegate tasks", true, []string{"test-model"})
	if err != nil {
		t.Fatal(err)
	}
	findCreate := func(agent *Agent) Tool {
		t.Helper()
		for _, tool := range agent.Tools {
			if tool.Name == "create_agent" {
				return tool
			}
		}
		t.Fatal("missing create_agent tool")
		return Tool{}
	}
	create := findCreate(parent)
	result, err := create.Execute(ctx, json.RawMessage(`{"name":"researcher","model":"test-model","instructions":"Research and reply to the sender"}`))
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]string
	if err := json.Unmarshal([]byte(result), &receipt); err != nil || receipt["name"] != "researcher" || receipt["status"] != "created" {
		t.Fatalf("invalid receipt: %s %v", result, err)
	}
	if len(env.Agents) != 2 || env.InitialAgent != parent {
		t.Fatal("creation changed initial agent or failed to add child")
	}
	child := env.Agents[1]
	if child.Instructions != "Research and reply to the sender" || len(child.ToolNames) != 0 || len(child.Tools) != 6 {
		t.Fatal("child did not get requested instructions and only automatic tools")
	}
	for _, test := range []struct {
		arguments string
		want      error
	}{
		{`{"name":"researcher","model":"test-model"}`, ErrAgentExists},
		{`{"name":"../invalid","model":"test-model"}`, ErrInvalidName},
		{`{"name":"invalid-model","model":"missing"}`, ErrModelNotAllowed},
	} {
		if _, err := create.Execute(ctx, json.RawMessage(test.arguments)); !errors.Is(err, test.want) {
			t.Fatalf("%s: %v", test.arguments, err)
		} else if errors.Is(err, ErrModelNotAllowed) && !strings.Contains(err.Error(), `"test-model"`) {
			t.Fatalf("missing configured model choices: %v", err)
		}
	}
	if _, err := create.Execute(ctx, json.RawMessage(`{`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := create.Execute(cancelled, json.RawMessage(`{"name":"cancelled","model":"test-model"}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	loaded, err := LoadAgentEnvironment(ctx, root, "team", registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Agents) != 2 || loaded.Agents[1].Session.SessionID != child.Session.SessionID {
		t.Fatal("child not persisted")
	}
	if _, err := findCreate(loaded.InitialAgent).Execute(ctx, json.RawMessage(`{"name":"restored-child","model":"test-model"}`)); err != nil {
		t.Fatal(err)
	}
	if len(loaded.Agents) != 3 || len(env.Agents) != 2 {
		t.Fatal("restored tool bound to wrong environment")
	}
	if DefaultTools()["create_agent"].Automatic {
		t.Fatal("creation tool registered as automatic")
	}
	models, err := ListModels()
	if err != nil {
		t.Fatal(err)
	}
	models = append(models, Model{Name: "new-model", Provider: ProviderOpenAI, Endpoint: "http://localhost"})
	if err := writeJSONAtomic("models.json", map[string]any{"models": models}); err != nil {
		t.Fatal(err)
	}
	if _, err := create.Execute(ctx, json.RawMessage(`{"name":"unknown-model-child","model":"new-model"}`)); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("newly configured model was automatically allowed: %v", err)
	}
	if err := env.SetAgentTools(ctx, parent.Name, []Tool{registry["create_agent"]}); err != nil {
		t.Fatal(err)
	}
	refreshed := findCreate(parent)
	if !reflect.DeepEqual(refreshed.Parameters[1].Enum, []string{"test-model"}) {
		t.Fatalf("rebinding lost model restrictions: %v", refreshed.Parameters[1].Enum)
	}
	if !reflect.DeepEqual(create.Parameters[1].Enum, []string{"test-model"}) {
		t.Fatal("rebinding mutated an existing tool definition")
	}
}

func TestCreateAgentToolAssignments(t *testing.T) {
	ctx, root, registry := setupPersistenceTest(t)
	registry["create_agent"], registry["get_time"] = CreateAgentTool(), DefaultTools()["get_time"]
	env, err := NewAgentEnvironment(ctx, root, "team")
	if err != nil {
		t.Fatal(err)
	}
	usable := []Tool{registry["create_agent"], registry["echo"]}
	assignable := []Tool{registry["get_time"], registry["create_agent"]}
	parent, err := env.CreateAgentWithModels(ctx, "test-model", usable, "parent", "", true, []string{"test-model"}, assignable...)
	if err != nil {
		t.Fatal(err)
	}
	find := func(agent *Agent, name string) Tool {
		t.Helper()
		for _, tool := range agent.Tools {
			if tool.Name == name {
				return tool
			}
		}
		t.Fatalf("missing tool %s", name)
		return Tool{}
	}
	create := find(parent, "create_agent")
	if !strings.Contains(create.Description, `"name":"get_time"`) {
		t.Fatal("grant-only tool not discoverable")
	}
	for _, tool := range parent.Tools {
		if tool.Name == "get_time" {
			t.Fatal("assign permission granted use permission")
		}
	}
	schema, err := json.Marshal(create.AsOpenAITool())
	if err != nil {
		t.Fatal(err)
	}
	var definition struct {
		Function struct {
			Parameters struct {
				Properties map[string]struct {
					Type  string
					Enum  []string
					Items struct{ Type string }
				}
			}
		}
	}
	if err := json.Unmarshal(schema, &definition); err != nil {
		t.Fatal(err)
	}
	if property := definition.Function.Parameters.Properties["tools"]; property.Type != "array" || property.Items.Type != "string" {
		t.Fatalf("invalid tools schema: %s", schema)
	}
	if property := definition.Function.Parameters.Properties["model"]; property.Type != "string" || !reflect.DeepEqual(property.Enum, []string{"test-model"}) {
		t.Fatalf("model choices do not match configured models: %s", schema)
	}
	if len(registry["create_agent"].Parameters[1].Enum) != 0 {
		t.Fatal("binding mutated the shared tool definition")
	}
	if _, err := create.Execute(ctx, json.RawMessage(`{"name":"child","model":"test-model","tools":["get_time","create_agent"]}`)); err != nil {
		t.Fatal(err)
	}
	child := env.Agents[1]
	if !reflect.DeepEqual(child.ToolNames, []string{"get_time", "create_agent"}) || len(child.AssignableTools) != 0 {
		t.Fatal("child use/assign permissions incorrect")
	}
	if _, err := find(child, "get_time").Execute(ctx, json.RawMessage(`{"location":"UTC"}`)); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range []string{
		`{"name":"denied","model":"test-model","tools":["echo"]}`,
		`{"name":"denied","model":"test-model","tools":["missing"],"caller":"parent"}`,
		`{"name":"denied","model":"test-model","tools":["get_time","get_time"]}`,
		`{"name":"denied","model":"test-model","tools":["message"]}`,
	} {
		if _, err := create.Execute(ctx, json.RawMessage(arguments)); !errors.Is(err, ErrInvalidTool) {
			t.Fatalf("invalid grant accepted: %s: %v", arguments, err)
		}
	}
	if _, err := create.Execute(ctx, json.RawMessage(`{"name":"denied","model":"test-model","tools":"get_time"}`)); err == nil {
		t.Fatal("non-array accepted")
	}
	if _, err := find(child, "create_agent").Execute(ctx, json.RawMessage(`{"name":"denied","model":"test-model","tools":["get_time"],"caller":"parent"}`)); !errors.Is(err, ErrInvalidTool) {
		t.Fatalf("child borrowed parent grants: %v", err)
	}
	if len(env.Agents) != 2 {
		t.Fatal("rejected request created an agent")
	}
	loaded, err := LoadAgentEnvironment(ctx, root, "team", registry)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.InitialAgent.AssignableToolNames, []string{"get_time", "create_agent"}) {
		t.Fatal("grant permissions not restored")
	}
	if _, err := find(loaded.InitialAgent, "create_agent").Execute(ctx, json.RawMessage(`{"name":"restored-child","model":"test-model","tools":["get_time"]}`)); err != nil {
		t.Fatal(err)
	}
	if err := env.SetAgentTools(ctx, "parent", usable); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(find(parent, "create_agent").Description, `"name":"get_time"`) {
		t.Fatal("use-only update lost grant catalogue")
	}
	previousRoot := env.DataRoot
	env.DataRoot = filepath.Join(root, "blocked")
	if err := os.WriteFile(env.DataRoot, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = env.UpdateAgent(ctx, "parent", "test-model", "", usable)
	env.DataRoot = previousRoot
	if err == nil || !reflect.DeepEqual(parent.AssignableToolNames, []string{"get_time", "create_agent"}) || !reflect.DeepEqual(parent.AllowedModels, []string{"test-model"}) {
		t.Fatal("failed update lost permissions")
	}
	if err := env.UpdateAgentWithModels(ctx, "parent", "test-model", "", usable, []string{"test-model"}); err != nil {
		t.Fatal(err)
	}
	if _, err := create.Execute(ctx, json.RawMessage(`{"name":"revoked","model":"test-model","tools":["get_time"]}`)); !errors.Is(err, ErrInvalidTool) {
		t.Fatalf("stale closure retained revoked grants: %v", err)
	}
	if _, err := create.Execute(ctx, json.RawMessage(`{"name":"empty","model":"test-model","tools":[]}`)); err != nil {
		t.Fatal(err)
	}
	if err := env.SetAgentTools(ctx, "parent", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := create.Execute(ctx, json.RawMessage(`{"name":"revoked","model":"test-model"}`)); !errors.Is(err, ErrInvalidTool) {
		t.Fatalf("revoked creation still allowed: %v", err)
	}
}

func TestCreateAgentModelPermissions(t *testing.T) {
	ctx, root, registry := setupPersistenceTest(t)
	registry["create_agent"] = CreateAgentTool()
	models, err := ListModels()
	if err != nil {
		t.Fatal(err)
	}
	models = append(models, Model{Name: "child-model", Provider: ProviderOpenAI, Endpoint: "http://localhost"})
	if err := writeJSONAtomic("models.json", map[string]any{"models": models}); err != nil {
		t.Fatal(err)
	}
	env, err := NewAgentEnvironment(ctx, root, "models")
	if err != nil {
		t.Fatal(err)
	}
	tools := []Tool{registry["create_agent"]}
	parent, err := env.CreateAgent(ctx, "test-model", tools, "parent", "", true, tools...)
	if err != nil {
		t.Fatal(err)
	}
	creation := func(agent *Agent) Tool {
		for _, tool := range agent.Tools {
			if tool.Name == "create_agent" {
				return tool
			}
		}
		t.Fatal("missing create_agent")
		return Tool{}
	}
	if _, err := creation(parent).Execute(ctx, json.RawMessage(`{"name":"denied","model":"test-model"}`)); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("empty list allowed creation: %v", err)
	}
	for _, names := range [][]string{{"missing"}, {"child-model", "child-model"}} {
		if err := env.UpdateAgentWithModels(ctx, "parent", "test-model", "", tools, names, tools...); err == nil {
			t.Fatal("invalid model list accepted")
		}
	}
	if err := env.UpdateAgentWithModels(ctx, "parent", "test-model", "", tools, []string{"child-model"}, tools...); err != nil {
		t.Fatal(err)
	}
	create := creation(parent)
	if !reflect.DeepEqual(create.Parameters[1].Enum, []string{"child-model"}) {
		t.Fatal("schema exposed models outside allowlist")
	}
	if _, err := create.Execute(ctx, json.RawMessage(`{"name":"denied","model":"test-model"}`)); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("parent model implicitly permitted: %v", err)
	}
	if _, err := create.Execute(ctx, json.RawMessage(`{"name":"child","model":"child-model","tools":["create_agent"]}`)); err != nil {
		t.Fatal(err)
	}
	child := env.agentLocked("child")
	if !reflect.DeepEqual(child.AllowedModels, parent.AllowedModels) || len(child.AssignableTools) != 0 {
		t.Fatal("child permissions not inherited correctly")
	}
	if _, err := creation(child).Execute(ctx, json.RawMessage(`{"name":"denied","model":"test-model","caller":"parent"}`)); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("child exceeded inherited limits: %v", err)
	}
	if _, err := creation(child).Execute(ctx, json.RawMessage(`{"name":"grandchild","model":"child-model"}`)); err != nil {
		t.Fatal(err)
	}
	if len(env.agentLocked("grandchild").AllowedModels) != 0 {
		t.Fatal("child without create_agent received model permissions")
	}
	loaded, err := LoadAgentEnvironment(ctx, root, "models", registry)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.agentLocked("child").AllowedModels, []string{"child-model"}) || !reflect.DeepEqual(creation(loaded.InitialAgent).Parameters[1].Enum, []string{"child-model"}) {
		t.Fatal("permissions lost on reload")
	}
	if err := env.UpdateAgent(ctx, "parent", "test-model", "", tools, tools...); err != nil {
		t.Fatal(err)
	}
	if _, err := create.Execute(ctx, json.RawMessage(`{"name":"revoked","model":"child-model"}`)); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("stale closure retained model permissions: %v", err)
	}
	if !reflect.DeepEqual(child.AllowedModels, []string{"child-model"}) {
		t.Fatal("parent revocation changed independent child permissions")
	}
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
	for _, tools := range [][]Tool{{registry["echo"], registry["echo"]}, {{Name: "invalid"}}, {DefaultTools()["search_memory"]}} {
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
	if len(agent.ToolNames) != 0 || len(agent.Tools) != 6 {
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
	if len(restored.Tools) != 7 {
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

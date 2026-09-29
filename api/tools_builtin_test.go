package api

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestNativeToolValidation(t *testing.T) {
	binding := ToolContext{Environment: &AgentEnvironment{}, Caller: "caller"}
	for name, definition := range DefaultTools() {
		t.Run(name, func(t *testing.T) {
			tool := definition.bind(binding)
			for _, raw := range []string{"", "null", "[]", `"text"`, "{", "{} {}", `{"text":"` + strings.Repeat("x", 128<<10) + `"}`} {
				if _, err := tool.Execute(t.Context(), json.RawMessage(raw)); err == nil {
					t.Fatalf("accepted invalid arguments: %.40s", raw)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := tool.Execute(ctx, json.RawMessage(`{}`)); !errors.Is(err, context.Canceled) {
				t.Fatalf("ignored cancellation: %v", err)
			}
			unbound := definition.bind(ToolContext{})
			if _, err := unbound.Execute(t.Context(), json.RawMessage(`{}`)); err == nil {
				t.Fatal("accepted missing caller context")
			}
		})
	}
}

func TestNativeToolResults(t *testing.T) {
	ctx, root, _ := setupPersistenceTest(t)
	t.Setenv("PATH", t.TempDir())
	env, err := NewAgentEnvironment(ctx, root, "native")
	if err != nil {
		t.Fatal(err)
	}
	env.MemoryStore.EmbeddingModel = ""
	agent, err := env.CreateAgent(ctx, "test-model", nil, "caller", "", true)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]Tool{}
	for _, tool := range agent.currentTools() {
		tools[tool.Name] = tool
	}
	for _, test := range []struct {
		name, arguments, want string
	}{
		{"list_agents", `{}`, `["caller"]`},
		{"write_memory", `{"kind":"fact","content":"Native memory"}`, "Written to memory: 'Native memory'"},
	} {
		result, err := tools[test.name].Execute(ctx, json.RawMessage(test.arguments))
		if err != nil || result != test.want {
			t.Fatalf("%s: got %q, %v; want %q", test.name, result, err, test.want)
		}
	}
	if _, err := tools["agent_status"].Execute(ctx, json.RawMessage(`{"name":"missing"}`)); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("lost native error identity: %v", err)
	}
	if _, err := tools["write_memory"].Execute(ctx, json.RawMessage(`{"kind":"fact","content":"`+strings.Repeat("x", 128<<10)+`"}`)); err == nil {
		t.Fatal("oversized arguments accepted")
	}
	if len(env.MemoryStore.entries) != 1 {
		t.Fatal("rejected arguments changed memory")
	}
	env.MemoryStore.entries = append(env.MemoryStore.entries, Memory{Content: strings.Repeat("x", 1<<20)})
	if result, err := tools["search_memory"].Execute(ctx, json.RawMessage(`{"query":"x"}`)); err == nil || result != "" {
		t.Fatalf("oversized native result accepted: %d bytes, %v", len(result), err)
	}
}

func TestNativeCreateAgentSchemaRefresh(t *testing.T) {
	ctx, root, _ := setupPersistenceTest(t)
	env, err := NewAgentEnvironment(ctx, root, "schema")
	if err != nil {
		t.Fatal(err)
	}
	definition := CreateAgentTool()
	agent, err := env.CreateAgentWithModels(ctx, "test-model", []Tool{definition}, "caller", "", true, []string{"test-model"})
	if err != nil {
		t.Fatal(err)
	}
	for _, configured := range []bool{true, false, true} {
		models := []Model{}
		var want []string
		if configured {
			models = append(models, Model{Name: "test-model", Provider: ProviderOpenAI})
			want = []string{"test-model"}
		}
		if err := writeJSONAtomic("models.json", map[string]any{"models": models}); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, tool := range agent.currentTools() {
			if tool.Name == "create_agent" {
				found = true
				if !reflect.DeepEqual(tool.Parameters[1].Enum, want) || strings.Count(tool.Description, "Tools you may assign:") != 1 {
					t.Fatalf("stale or accumulated binding: %+v", tool)
				}
			}
		}
		if !found {
			t.Fatal("missing native create_agent")
		}
	}
	if len(definition.Parameters[1].Enum) != 0 {
		t.Fatal("binding mutated the shared definition")
	}
}

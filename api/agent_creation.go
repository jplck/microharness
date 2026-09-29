package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

func CreateAgentTool() Tool {
	tool := Tool{
		Name:        "create_agent",
		Description: "Create an agent in your environment using a configured model name. Optionally grant tools from your Assign permissions, independently of tools you can use. Children receive memory and messaging tools automatically, but no Assign permissions. Use message afterwards to delegate work and request a reply.",
		Parameters: []Parameter{
			{Name: "name", Type: String, Description: "Unique agent name using letters, digits, underscores, or hyphens", Required: true},
			{Name: "model", Type: String, Description: "Configured model name for the new agent", Required: true},
			{Name: "instructions", Type: String, Description: "Role and working instructions for the new agent"},
			{Name: "tools", Type: Array, Items: String, Description: "Optional tool names to grant for use. Must be in your Assign permissions; omitted or empty grants no optional tools."},
		},
	}
	tool.bindEnvironment = func(env *AgentEnvironment, caller string, assignableTools []Tool) Tool {
		bound := tool
		catalogue := make([]ToolSummary, 0, len(assignableTools))
		for _, allowed := range assignableTools {
			catalogue = append(catalogue, ToolSummary{Name: allowed.Name, Description: allowed.Description})
		}
		encoded, _ := json.Marshal(catalogue)
		bound.Description += " Tools you may assign: " + string(encoded)
		bound.Execute = JSONHandler(func(ctx context.Context, arguments struct {
			Name         string   `json:"name"`
			Model        string   `json:"model"`
			Instructions string   `json:"instructions"`
			Tools        []string `json:"tools"`
		}) (string, error) {
			env.mu.Lock()
			defer env.mu.Unlock()
			parent := env.agentLocked(caller)
			if parent == nil || !slices.Contains(parent.ToolNames, "create_agent") {
				return "", fmt.Errorf("%w: caller cannot use create_agent", ErrInvalidTool)
			}
			tools := make([]Tool, 0, len(arguments.Tools))
			for _, name := range arguments.Tools {
				index := slices.IndexFunc(parent.AssignableTools, func(candidate Tool) bool { return candidate.Name == name })
				if index < 0 {
					return "", fmt.Errorf("%w: caller cannot assign %q", ErrInvalidTool, name)
				}
				tools = append(tools, parent.AssignableTools[index])
			}
			_, err := env.createAgentLocked(ctx, arguments.Model, tools, arguments.Name, arguments.Instructions, false)
			if err != nil {
				return "", err
			}
			result, err := json.Marshal(map[string]string{"name": arguments.Name, "status": "created"})
			return string(result), err
		})
		return bound
	}
	return tool
}

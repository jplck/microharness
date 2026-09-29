package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

var ErrModelNotAllowed = errors.New("model not allowed for child creation")

func validateAllowedModels(names []string) error {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			return fmt.Errorf("%w: duplicate %q", ErrModelNotAllowed, name)
		}
		if _, err := GetModelByName(name); err != nil {
			return err
		}
		seen[name] = true
	}
	return nil
}

func CreateAgentTool() Tool {
	tool := Tool{
		Name:        "create_agent",
		Description: "Create an agent in your environment using a configured model name. Optionally grant tools from your Assign permissions, independently of tools you can use. Children receive memory and messaging tools automatically, but no Assign permissions. Use message afterwards to delegate work and request a reply.",
		Parameters: []Parameter{
			{Name: "name", Type: String, Description: "Unique agent name using letters, digits, underscores, or hyphens", Required: true},
			{Name: "model", Type: String, Description: "Exact configured model name for the new agent. Choose from the listed values; do not invent model names.", Required: true},
			{Name: "instructions", Type: String, Description: "Role and working instructions for the new agent"},
			{Name: "tools", Type: Array, Items: String, Description: "Optional tool names to grant for use. Must be in your Assign permissions; omitted or empty grants no optional tools."},
		},
	}
	tool.bindEnvironment = func(env *AgentEnvironment, caller string, assignableTools []Tool, allowedModels []string) Tool {
		bound := tool
		bound.Parameters = slices.Clone(tool.Parameters)
		if models, err := ListModels(); err == nil {
			for _, model := range models {
				if slices.Contains(allowedModels, model.Name) {
					bound.Parameters[1].Enum = append(bound.Parameters[1].Enum, model.Name)
				}
			}
		}
		if len(bound.Parameters[1].Enum) == 0 {
			bound.Description += " No child models are allowed; do not call this tool until model permissions are configured."
		}
		bound.Description += " Children granted create_agent inherit your child-model allowlist, but no Assign tool permissions."
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
			if !slices.Contains(parent.AllowedModels, arguments.Model) {
				return "", fmt.Errorf("%w: %q; choose from %q", ErrModelNotAllowed, arguments.Model, parent.AllowedModels)
			}
			tools := make([]Tool, 0, len(arguments.Tools))
			for _, name := range arguments.Tools {
				index := slices.IndexFunc(parent.AssignableTools, func(candidate Tool) bool { return candidate.Name == name })
				if index < 0 {
					return "", fmt.Errorf("%w: caller cannot assign %q", ErrInvalidTool, name)
				}
				tools = append(tools, parent.AssignableTools[index])
			}
			var childModels []string
			if slices.Contains(arguments.Tools, "create_agent") {
				childModels = parent.AllowedModels
			}
			_, err := env.createAgentLocked(ctx, arguments.Model, tools, arguments.Name, arguments.Instructions, false, childModels)
			if err != nil {
				if errors.Is(err, ErrModelNotFound) {
					if models, listErr := ListModels(); listErr == nil {
						names := make([]string, 0, len(models))
						for _, model := range models {
							if slices.Contains(parent.AllowedModels, model.Name) {
								names = append(names, model.Name)
							}
						}
						return "", fmt.Errorf("%w; choose an exact configured model name from %q", err, names)
					}
				}
				return "", err
			}
			result, err := json.Marshal(map[string]string{"name": arguments.Name, "status": "created"})
			return string(result), err
		})
		return bound
	}
	return tool
}

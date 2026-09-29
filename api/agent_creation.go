package api

import (
	"context"
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

type ChildAgentRequest struct {
	Name         string
	Model        string
	Instructions string
	Tools        []string
}

func (env *AgentEnvironment) CreateChildAgent(ctx context.Context, caller string, request ChildAgentRequest) (*Agent, error) {
	env.mu.Lock()
	defer env.mu.Unlock()
	parent := env.agentLocked(caller)
	if parent == nil || !slices.Contains(parent.ToolNames, "create_agent") {
		return nil, fmt.Errorf("%w: caller cannot use create_agent", ErrInvalidTool)
	}
	if !slices.Contains(parent.AllowedModels, request.Model) {
		return nil, fmt.Errorf("%w: %q; choose from %q", ErrModelNotAllowed, request.Model, parent.AllowedModels)
	}
	tools := make([]Tool, 0, len(request.Tools))
	for _, name := range request.Tools {
		index := slices.IndexFunc(parent.AssignableTools, func(candidate Tool) bool { return candidate.Name == name })
		if index < 0 {
			return nil, fmt.Errorf("%w: caller cannot assign %q", ErrInvalidTool, name)
		}
		tools = append(tools, parent.AssignableTools[index])
	}
	var childModels []string
	if slices.Contains(request.Tools, "create_agent") {
		childModels = parent.AllowedModels
	}
	child, err := env.createAgentLocked(ctx, request.Model, tools, request.Name, request.Instructions, false, childModels)
	if errors.Is(err, ErrModelNotFound) {
		if models, listErr := ListModels(); listErr == nil {
			names := make([]string, 0, len(models))
			for _, model := range models {
				if slices.Contains(parent.AllowedModels, model.Name) {
					names = append(names, model.Name)
				}
			}
			return nil, fmt.Errorf("%w; choose an exact configured model name from %q", err, names)
		}
	}
	return child, err
}

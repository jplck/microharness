package api

import (
	"fmt"
	"slices"
)

type ToolRegistry map[string]Tool

func CreateAgentTool() Tool { return DefaultTools()["create_agent"] }

func (registry ToolRegistry) Optional() []ToolSummary {
	names := make([]string, 0, len(registry))
	for name, tool := range registry {
		if !tool.Automatic {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	tools := make([]ToolSummary, 0, len(names))
	for _, name := range names {
		tools = append(tools, ToolSummary{Name: name, Description: registry[name].Description})
	}
	return tools
}

func (registry ToolRegistry) Resolve(names []string) ([]Tool, error) {
	tools := make([]Tool, 0, len(names))
	for _, name := range names {
		tool, ok := registry[name]
		if !ok || tool.Automatic || tool.Name != name || (tool.Execute == nil && tool.bind == nil) {
			return nil, fmt.Errorf("%w %q", ErrInvalidTool, name)
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func (registry ToolRegistry) Bind(binding ToolContext, tools []Tool) ([]Tool, []string, error) {
	if binding.Environment == nil || binding.Caller == "" {
		return nil, nil, fmt.Errorf("%w: environment and caller are required", ErrInvalidTool)
	}
	binding.AssignableTools = slices.Clone(binding.AssignableTools)
	binding.AllowedModels = slices.Clone(binding.AllowedModels)
	automaticNames := []string{}
	for name, tool := range registry {
		if tool.Automatic {
			automaticNames = append(automaticNames, name)
		}
	}
	slices.Sort(automaticNames)
	resolved := make([]Tool, 0, len(automaticNames)+len(tools))
	seen := make(map[string]bool)
	for _, name := range automaticNames {
		tool := registry[name]
		if tool.bind != nil {
			tool = tool.bind(binding)
		}
		if tool.Name != name || tool.Execute == nil {
			return nil, nil, fmt.Errorf("%w %q", ErrInvalidTool, name)
		}
		tool.Automatic = true
		resolved = append(resolved, tool)
		seen[name] = true
	}
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool.Automatic || tool.Name == "" || seen[tool.Name] {
			return nil, nil, fmt.Errorf("%w %q", ErrInvalidTool, tool.Name)
		}
		if tool.bind != nil {
			tool = tool.bind(binding)
		}
		if tool.Execute == nil {
			return nil, nil, fmt.Errorf("%w %q", ErrInvalidTool, tool.Name)
		}
		seen[tool.Name] = true
		names = append(names, tool.Name)
		resolved = append(resolved, tool)
	}
	return resolved, names, nil
}

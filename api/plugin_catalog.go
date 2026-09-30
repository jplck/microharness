package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jplck/micro/toolplugin"
)

var pluginToolName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func readPlugin(ctx context.Context, path string) (*pluginBinary, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp("", "micro-describe-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	output, err := runPluginCommand(ctx, 10*time.Second, absolute, []string{"describe"}, directory, []string{"PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"}, nil)
	if err != nil {
		return nil, fmt.Errorf("describe %s: %w", path, err)
	}
	var manifest toolplugin.Manifest
	if err := json.Unmarshal(output, &manifest); err != nil {
		return nil, fmt.Errorf("plugin manifest: %w", err)
	}
	if manifest.ProtocolVersion != 1 || len(manifest.Tools) == 0 || len(manifest.Tools) > 64 {
		return nil, fmt.Errorf("invalid plugin manifest version or tool count")
	}
	seen := map[string]bool{}
	for _, definition := range manifest.Tools {
		if !pluginToolName.MatchString(definition.Name) || seen[definition.Name] || strings.TrimSpace(definition.Description) == "" || len(definition.Description) > 8192 || definition.Automatic {
			return nil, fmt.Errorf("%w: plugin definition %q", ErrInvalidTool, definition.Name)
		}
		seen[definition.Name] = true
		parameters := map[string]bool{}
		for _, parameter := range definition.Parameters {
			if !pluginToolName.MatchString(parameter.Name) || parameters[parameter.Name] {
				return nil, fmt.Errorf("%w: parameter %q", ErrInvalidTool, parameter.Name)
			}
			parameters[parameter.Name] = true
			primitive := func(kind ParameterType) bool {
				return kind == String || kind == Integer || kind == Number || kind == Boolean
			}
			if !primitive(parameter.Type) && (parameter.Type != Array || !primitive(parameter.Items)) {
				return nil, fmt.Errorf("%w: parameter type %q", ErrInvalidTool, parameter.Type)
			}
			if (parameter.Type != Array && parameter.Items != "") || (len(parameter.Enum) > 0 && parameter.Type != String) {
				return nil, fmt.Errorf("%w: parameter schema %q", ErrInvalidTool, parameter.Name)
			}
		}
	}
	return &pluginBinary{Path: absolute, Definitions: manifest.Tools}, nil
}

func (env *AgentEnvironment) toolRegistryLocked() ToolRegistry {
	registry := DefaultTools()
	for name, tool := range env.registry {
		registry[name] = tool
	}
	return registry
}

func (agent *Agent) currentTools() []Tool {
	if agent.environment == nil {
		return agent.Tools
	}
	env := agent.environment
	env.mu.Lock()
	defer env.mu.Unlock()
	binding := ToolContext{Environment: env, Caller: agent.Name, AssignableTools: agent.AssignableTools, AllowedModels: agent.AllowedModels}
	tools := make([]Tool, 0, len(agent.Tools))
	for _, tool := range agent.Tools {
		if tool.Name == "update_tool" && !slices.Contains(agent.ToolNames, "create_tool") {
			continue
		}
		if tool.bind != nil {
			tool = tool.bind(binding)
		}
		tools = append(tools, tool)
	}
	if slices.Contains(agent.ToolNames, "create_tool") {
		for _, plugin := range agent.plugins {
			for _, definition := range plugin.Binary.Definitions {
				tools = append(tools, binaryTool(plugin.Binary, definition))
			}
		}
	}
	return tools
}

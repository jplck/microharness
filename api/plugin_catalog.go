package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

func loadPluginCatalogue(ctx context.Context, pluginDirectory string) (ToolRegistry, string, error) {
	cache, err := os.MkdirTemp("", "micro-catalogue-*")
	if err != nil {
		return nil, "", err
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(cache)
		}
	}()
	registry := DefaultTools()
	load := func(path string) error {
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		info, err := input.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return fmt.Errorf("plugin must be an executable regular file: %s", path)
		}
		output, err := os.CreateTemp(cache, "binary-*")
		if err != nil {
			return err
		}
		if _, err := io.Copy(output, input); err != nil {
			output.Close()
			return err
		}
		if err := output.Chmod(0700); err != nil {
			output.Close()
			return err
		}
		if err := output.Close(); err != nil {
			return err
		}
		binary, err := readPlugin(ctx, output.Name())
		if err != nil {
			return err
		}
		for _, definition := range binary.Definitions {
			if _, exists := registry[definition.Name]; exists {
				return fmt.Errorf("%w: duplicate plugin tool %q", ErrInvalidTool, definition.Name)
			}
			registry[definition.Name] = binaryTool(binary, definition)
		}
		return nil
	}
	entries, err := os.ReadDir(pluginDirectory)
	if err != nil && !os.IsNotExist(err) {
		return nil, "", err
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path, err := filepath.Abs(filepath.Join(pluginDirectory, entry.Name()))
		if err != nil {
			return nil, "", err
		}
		if err := load(path); err != nil {
			return nil, "", fmt.Errorf("load plugin %q: %w", entry.Name(), err)
		}
	}
	success = true
	return registry, cache, nil
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
		if tool.plugin != nil && env.registry != nil {
			if replacement, exists := env.registry[tool.Name]; exists {
				tool = replacement
			} else {
				continue
			}
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

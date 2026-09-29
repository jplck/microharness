package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jplck/micro/toolplugin"
)

type CreatePluginRequest struct {
	Name           string `json:"name"`
	Source         string `json:"source"`
	TestArguments  string `json:"test_arguments"`
	ExpectedOutput string `json:"expected_output"`
}

type ownedPlugin struct {
	Path   string
	Binary *pluginBinary
}

func buildPlugin(ctx context.Context, directory, source string) error {
	if err := os.MkdirAll(filepath.Join(directory, "toolplugin"), 0700); err != nil {
		return err
	}
	for name, content := range map[string]string{"go.mod": "module github.com/jplck/micro\n\ngo 1.25.0\n", "main.go": source, "toolplugin/plugin.go": toolplugin.Source} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0600); err != nil {
			return err
		}
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	environment := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "GOCACHE=" + filepath.Join(cache, "go-build"), "CGO_ENABLED=0", "GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOENV=off", "GOTOOLCHAIN=local"}
	_, err = runPluginCommand(ctx, 90*time.Second, "go", []string{"build", "-mod=readonly", "-trimpath", "-o", "tool", "."}, directory, environment, nil)
	return err
}

func (env *AgentEnvironment) pluginOwnerLocked(caller, name string) (*Agent, error) {
	if env.closed {
		return nil, ErrEnvironmentClosed
	}
	agent := env.agentLocked(caller)
	if agent == nil || !slices.Contains(agent.ToolNames, "create_tool") {
		return nil, fmt.Errorf("%w: caller cannot use create_tool", ErrInvalidTool)
	}
	if _, exists := env.toolRegistryLocked()[name]; exists {
		return nil, fmt.Errorf("%w: reserved tool name %q", ErrInvalidTool, name)
	}
	for _, tool := range agent.Tools {
		if tool.Name == name {
			return nil, fmt.Errorf("%w: existing tool %q", ErrInvalidTool, name)
		}
	}
	for _, plugin := range agent.plugins {
		for _, definition := range plugin.Binary.Definitions {
			if definition.Name == name {
				return nil, fmt.Errorf("%w: existing private tool %q", ErrInvalidTool, name)
			}
		}
	}
	return agent, nil
}

func (env *AgentEnvironment) CreatePlugin(ctx context.Context, caller string, request CreatePluginRequest) error {
	if !pluginToolName.MatchString(request.Name) || strings.TrimSpace(request.Source) == "" || len(request.Source) > 64<<10 {
		return fmt.Errorf("%w: invalid tool name or Go source (max 64 KiB)", ErrInvalidTool)
	}
	var arguments map[string]json.RawMessage
	if len(request.TestArguments) > 64<<10 || json.Unmarshal([]byte(request.TestArguments), &arguments) != nil || arguments == nil {
		return fmt.Errorf("test_arguments must encode a JSON object, max 64 KiB")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	env.mu.Lock()
	_, err := env.pluginOwnerLocked(caller, request.Name)
	root := filepath.Join(env.DataRoot, "plugins")
	env.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	directory, err := os.MkdirTemp(root, ".build-*")
	if err != nil {
		return err
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return err
	}
	defer func() { os.RemoveAll(directory) }()
	if err := buildPlugin(ctx, directory, request.Source); err != nil {
		return fmt.Errorf("build plugin: %w", err)
	}
	binary, err := readPlugin(ctx, filepath.Join(directory, "tool"), false)
	if err != nil {
		return err
	}
	if len(binary.Definitions) != 1 || binary.Definitions[0].Name != request.Name {
		return fmt.Errorf("plugin must describe exactly the requested tool %q", request.Name)
	}
	result, err := binary.call(ctx, request.Name, json.RawMessage(request.TestArguments), ToolContext{})
	if err != nil {
		return fmt.Errorf("plugin test: %w", err)
	}
	if result != request.ExpectedOutput {
		return fmt.Errorf("plugin test: got %q, expected %q", result, request.ExpectedOutput)
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	agent, err := env.pluginOwnerLocked(caller, request.Name)
	if err != nil {
		return err
	}
	name := caller + "-" + rand.Text()
	final, err := filepath.Abs(filepath.Join(root, name))
	if err != nil {
		return err
	}
	if err := os.Rename(directory, final); err != nil {
		return err
	}
	binary.Path = filepath.Join(final, "tool")
	previous := agent.plugins
	agent.plugins = append(agent.plugins, ownedPlugin{Path: name, Binary: binary})
	if err := env.saveLocked(); err != nil {
		agent.plugins = previous
		os.RemoveAll(final)
		return err
	}
	return nil
}

func checkPrivatePluginNames(agent *Agent, registry ToolRegistry, selected ...Tool) error {
	seen := map[string]bool{}
	for _, tool := range selected {
		seen[tool.Name] = true
	}
	for _, plugin := range agent.plugins {
		for _, definition := range plugin.Binary.Definitions {
			if _, exists := registry[definition.Name]; exists || seen[definition.Name] {
				return fmt.Errorf("%w: private tool %q conflicts with catalogue", ErrInvalidTool, definition.Name)
			}
			seen[definition.Name] = true
		}
	}
	return nil
}

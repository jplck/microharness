package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jplck/micro/toolplugin"
)

type CreatePluginRequest struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	// TestSource must exercise a successful handler call in TestToolSuccess.
	// Execution is verified; arbitrary Go tests cannot prove semantic correctness.
	TestSource     string `json:"test_source"`
	TestArguments  string `json:"test_arguments"`
	ExpectedOutput string `json:"expected_output"`
}

// ErrPluginUpdateConflict means the captured owner or plugin version is no longer current.
var ErrPluginUpdateConflict = errors.New("private plugin changed during update")

type ownedPlugin struct {
	Path          string
	Binary        *pluginBinary
	PreviousPaths []string
}

func privatePluginPaths(root, saved string) (directory, executable string, err error) {
	parts := strings.Split(saved, "/")
	if len(parts) != 3 || !validStateName.MatchString(parts[0]) ||
		parts[1] != "bin" || !pluginToolName.MatchString(parts[2]) {
		return "", "", fmt.Errorf("invalid saved plugin path %q", saved)
	}
	directory = filepath.Join(root, "plugins", parts[0])
	return directory, filepath.Join(directory, "bin", parts[2]), nil
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
	environment, err := pluginBuildEnvironment(directory)
	if err != nil {
		return err
	}
	_, err = runPluginCommand(ctx, 90*time.Second, "go", []string{"build", "-mod=readonly", "-trimpath", "-o", "tool", "."}, directory, environment, nil)
	return err
}

func pluginBuildEnvironment(directory string) ([]string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	work := filepath.Join(directory, ".work")
	if err := os.MkdirAll(work, 0700); err != nil {
		return nil, err
	}
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "GOCACHE=" + filepath.Join(cache, "go-build"), "TMPDIR=" + work, "CGO_ENABLED=0", "GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOENV=off", "GOTOOLCHAIN=local"}, nil
}

func testPlugin(ctx context.Context, directory, source string) error {
	if err := os.WriteFile(filepath.Join(directory, "main_test.go"), []byte(source), 0600); err != nil {
		return err
	}
	environment, err := pluginBuildEnvironment(directory)
	if err != nil {
		return err
	}
	output, err := runPluginCommand(ctx, 90*time.Second, "go", []string{"test", "-json", "-count=1", "-mod=readonly", "-timeout=30s", "."}, directory, environment, nil)
	if err != nil {
		return fmt.Errorf("plugin success tests: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	ran, passed, packagePassed := false, false, false
	for {
		var event struct {
			Action  string
			Package string
			Test    string
		}
		if err := decoder.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("invalid plugin test report: %w", err)
		}
		if event.Package != "github.com/jplck/micro" {
			continue
		}
		if event.Action == "fail" || (event.Test == "TestToolSuccess" && event.Action == "skip") {
			return fmt.Errorf("TestToolSuccess must execute and pass without skipping")
		}
		if event.Test == "TestToolSuccess" {
			ran = ran || event.Action == "run"
			passed = passed || (ran && event.Action == "pass")
		}
		packagePassed = packagePassed || (event.Test == "" && event.Action == "pass")
	}
	if !ran || !passed || !packagePassed {
		return fmt.Errorf("TestToolSuccess must execute and pass without skipping")
	}
	return nil
}

func (env *AgentEnvironment) pluginOwnerLocked(caller, name string, update bool) (*Agent, int, error) {
	if env.closed {
		return nil, -1, ErrEnvironmentClosed
	}
	agent := env.agentLocked(caller)
	if agent == nil || !slices.Contains(agent.ToolNames, "create_tool") {
		return nil, -1, fmt.Errorf("%w: caller cannot use create_tool", ErrInvalidTool)
	}
	if _, exists := env.toolRegistryLocked()[name]; exists {
		return nil, -1, fmt.Errorf("%w: reserved tool name %q", ErrInvalidTool, name)
	}
	for _, tool := range agent.Tools {
		if tool.Name == name {
			return nil, -1, fmt.Errorf("%w: existing tool %q", ErrInvalidTool, name)
		}
	}
	for index, plugin := range agent.plugins {
		for _, definition := range plugin.Binary.Definitions {
			if definition.Name == name {
				if update {
					return agent, index, nil
				}
				return nil, -1, fmt.Errorf("%w: existing private tool %q", ErrInvalidTool, name)
			}
		}
	}
	if update {
		return nil, -1, fmt.Errorf("%w: no owned private tool %q", ErrInvalidTool, name)
	}
	return agent, -1, nil
}

func (env *AgentEnvironment) CreatePlugin(ctx context.Context, caller string, request CreatePluginRequest) error {
	return env.installPlugin(ctx, caller, request, false)
}

func (env *AgentEnvironment) UpdatePlugin(ctx context.Context, caller string, request CreatePluginRequest) error {
	return env.installPlugin(ctx, caller, request, true)
}

func (env *AgentEnvironment) installPlugin(ctx context.Context, caller string, request CreatePluginRequest, update bool) error {
	if !pluginToolName.MatchString(request.Name) || strings.TrimSpace(request.Source) == "" || len(request.Source) > 64<<10 {
		return fmt.Errorf("%w: invalid tool name or Go source (max 64 KiB)", ErrInvalidTool)
	}
	if strings.TrimSpace(request.TestSource) == "" || len(request.TestSource) > 64<<10 {
		return fmt.Errorf("%w: test_source is required Go package main test code (max 64 KiB), including TestToolSuccess", ErrInvalidTool)
	}
	testFile, err := parser.ParseFile(token.NewFileSet(), "main_test.go", request.TestSource, parser.PackageClauseOnly)
	if err != nil || testFile.Name.Name != "main" {
		return fmt.Errorf("%w: test_source must declare package main", ErrInvalidTool)
	}
	var arguments map[string]json.RawMessage
	if len(request.TestArguments) > 64<<10 || json.Unmarshal([]byte(request.TestArguments), &arguments) != nil || arguments == nil {
		return fmt.Errorf("test_arguments must encode a JSON object, max 64 KiB")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	env.mu.Lock()
	owner, index, err := env.pluginOwnerLocked(caller, request.Name, update)
	var original ownedPlugin
	if err == nil && update {
		original = owner.plugins[index]
	}
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
	if err := testPlugin(ctx, directory, request.TestSource); err != nil {
		return err
	}
	binary, err := readPlugin(ctx, filepath.Join(directory, "tool"))
	if err != nil {
		return err
	}
	if len(binary.Definitions) != 1 || binary.Definitions[0].Name != request.Name {
		return fmt.Errorf("plugin must describe exactly the requested tool %q", request.Name)
	}
	result, err := binary.call(ctx, request.Name, json.RawMessage(request.TestArguments))
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
	if update && env.agentLocked(caller) != owner {
		return ErrPluginUpdateConflict
	}
	agent, index, err := env.pluginOwnerLocked(caller, request.Name, update)
	if err != nil {
		return err
	}
	if agent != owner {
		return ErrAgentNotFound
	}
	if update && (agent.plugins[index].Binary != original.Binary || agent.plugins[index].Path != original.Path) {
		return ErrPluginUpdateConflict
	}
	if err := os.Mkdir(filepath.Join(directory, "bin"), 0700); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(directory, "tool"), filepath.Join(directory, "bin", request.Name)); err != nil {
		return err
	}
	name := caller + "-" + request.Name + "-" + rand.Text()
	final, err := filepath.Abs(filepath.Join(root, name))
	if err != nil {
		return err
	}
	if err := os.Rename(directory, final); err != nil {
		return err
	}
	binary.Path = filepath.Join(final, "bin", request.Name)
	previous := agent.plugins
	agent.plugins = slices.Clone(previous)
	replacement := ownedPlugin{Path: name + "/bin/" + request.Name, Binary: binary}
	if update {
		replacement.PreviousPaths = append(slices.Clone(original.PreviousPaths), original.Path)
		agent.plugins[index] = replacement
	} else {
		agent.plugins = append(agent.plugins, replacement)
	}
	if err := env.saveLocked(); err != nil {
		agent.plugins = previous
		os.RemoveAll(final)
		return err
	}
	// Existing tool snapshots still point at the old immutable executable.
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

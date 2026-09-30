package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

var validStateName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

type AgentEnvironment struct {
	registry     ToolRegistry
	mu           sync.Mutex
	workerCtx    context.Context
	workerCancel context.CancelFunc
	workerWG     sync.WaitGroup
	closed       bool
	Agents       []*Agent
	MemoryStore  *MemoryStore
	InitialAgent *Agent
	DataRoot     string
	ID           string
	Name         string
}

type EnvironmentState struct {
	Version          int          `json:"version"`
	ID               string       `json:"id"`
	Name             string       `json:"name"`
	EmbeddingModel   string       `json:"embedding_model"`
	InitialAgentName string       `json:"initial_agent_name,omitempty"`
	Agents           []AgentState `json:"agents"`
}

type AgentState struct {
	Name                string     `json:"name"`
	ModelName           string     `json:"model_name"`
	Instructions        string     `json:"instructions,omitempty"`
	SessionID           string     `json:"session_id"`
	ToolNames           []string   `json:"tools,omitempty"`
	AssignableToolNames []string   `json:"assignable_tools,omitempty"`
	AllowedModels       []string   `json:"allowed_models,omitempty"`
	Plugins             []string   `json:"plugins,omitempty"`
	Inbox               []Envelope `json:"inbox,omitempty"`
	InboxError          string     `json:"inbox_error,omitempty"`
	NotifiedFailures    []string   `json:"notified_failures,omitempty"`
}

func NewAgentEnvironment(ctx context.Context, dataRoot, name string) (*AgentEnvironment, error) {
	if !validStateName.MatchString(name) {
		return nil, fmt.Errorf("invalid environment name %q", name)
	}
	directory := filepath.Join(dataRoot, name)
	if _, err := os.Stat(filepath.Join(directory, "environment.json")); err == nil {
		return nil, fmt.Errorf("environment %q already exists: %w", name, os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	store, err := LoadMemories(ctx, filepath.Join(directory, "memory.json"))
	if err != nil {
		return nil, fmt.Errorf("load memories: %w", err)
	}
	store.EmbeddingModel = "nomic-embed-text"
	env := &AgentEnvironment{
		Agents: []*Agent{}, MemoryStore: store, DataRoot: directory,
		ID: rand.Text(), Name: name,
	}
	if err := env.saveLocked(); err != nil {
		return nil, err
	}
	return env, nil
}

func LoadAgentEnvironment(ctx context.Context, dataRoot, name string, registry ToolRegistry) (*AgentEnvironment, error) {
	if !validStateName.MatchString(name) {
		return nil, fmt.Errorf("invalid environment name %q", name)
	}
	directory := filepath.Join(dataRoot, name)
	data, err := os.ReadFile(filepath.Join(directory, "environment.json"))
	if err != nil {
		return nil, fmt.Errorf("read environment: %w", err)
	}
	var state EnvironmentState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode environment: %w", err)
	}
	if state.Version != 1 {
		return nil, fmt.Errorf("unsupported environment version %d", state.Version)
	}
	if state.ID == "" || state.Name != name {
		return nil, fmt.Errorf("invalid environment identity for %q", name)
	}
	store, err := LoadMemories(ctx, filepath.Join(directory, "memory.json"))
	if err != nil {
		return nil, fmt.Errorf("load memories: %w", err)
	}
	store.EmbeddingModel = state.EmbeddingModel
	env := &AgentEnvironment{
		Agents: make([]*Agent, 0, len(state.Agents)), MemoryStore: store,
		registry: registry,
		DataRoot: directory, ID: state.ID, Name: state.Name,
	}
	seen := make(map[string]bool)
	for _, saved := range state.Agents {
		if !validStateName.MatchString(saved.Name) || seen[saved.Name] || saved.SessionID == "" {
			return nil, fmt.Errorf("invalid or duplicate agent %q", saved.Name)
		}
		seen[saved.Name] = true
		tools, err := registry.Resolve(saved.ToolNames)
		if err != nil {
			return nil, fmt.Errorf("agent %q requires registered tools: %w", saved.Name, err)
		}
		assignableTools, err := registry.Resolve(saved.AssignableToolNames)
		if err != nil {
			return nil, fmt.Errorf("agent %q requires registered assignable tools: %w", saved.Name, err)
		}
		agent, err := env.createAgent(ctx, saved.ModelName, tools, saved.Name, saved.Instructions, saved.SessionID, saved.AllowedModels, assignableTools...)
		if err != nil {
			return nil, fmt.Errorf("restore agent %q: %w", saved.Name, err)
		}
		for _, name := range saved.Plugins {
			_, path, err := privatePluginPaths(directory, name)
			if err != nil {
				return nil, err
			}
			binary, err := readPlugin(ctx, path)
			if err != nil {
				return nil, fmt.Errorf("restore agent %q plugin: %w", saved.Name, err)
			}
			if len(binary.Definitions) != 1 || binary.Definitions[0].Name != filepath.Base(path) {
				return nil, fmt.Errorf("restore agent %q plugin: expected one tool matching the executable name", saved.Name)
			}
			agent.plugins = append(agent.plugins, ownedPlugin{Path: name, Binary: binary})
		}
		if err := checkPrivatePluginNames(agent, env.toolRegistryLocked()); err != nil {
			return nil, err
		}
		agent.Inbox = saved.Inbox
		agent.InboxError = saved.InboxError
		agent.notifiedFailures = saved.NotifiedFailures
		env.Agents = append(env.Agents, agent)
		if saved.Name == state.InitialAgentName {
			env.InitialAgent = agent
		}
	}
	if state.InitialAgentName != "" && env.InitialAgent == nil {
		return nil, fmt.Errorf("initial agent %q not found", state.InitialAgentName)
	}
	return env, nil
}

func (env *AgentEnvironment) saveLocked() error {
	state := EnvironmentState{
		Version: 1, ID: env.ID, Name: env.Name,
		EmbeddingModel: env.MemoryStore.EmbeddingModel,
		Agents:         make([]AgentState, 0, len(env.Agents)),
	}
	if env.InitialAgent != nil {
		state.InitialAgentName = env.InitialAgent.Name
	}
	for _, agent := range env.Agents {
		plugins := make([]string, 0, len(agent.plugins))
		for _, plugin := range agent.plugins {
			plugins = append(plugins, plugin.Path)
		}
		state.Agents = append(state.Agents, AgentState{
			Name: agent.Name, ModelName: agent.ModelName, Instructions: agent.Instructions,
			SessionID: agent.Session.SessionID, ToolNames: agent.ToolNames, Inbox: agent.Inbox,
			InboxError:          agent.InboxError,
			NotifiedFailures:    agent.notifiedFailures,
			AssignableToolNames: agent.AssignableToolNames,
			AllowedModels:       agent.AllowedModels,
			Plugins:             plugins,
		})
	}
	if err := writeJSONAtomic(filepath.Join(env.DataRoot, "environment.json"), state); err != nil {
		return fmt.Errorf("save environment: %w", err)
	}
	return nil
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

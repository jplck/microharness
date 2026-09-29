package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

func ServeRuntime(ctx context.Context, socketPath, dataRoot string) error {
	var mu sync.Mutex
	registry := BuiltinTools()
	registry["create_agent"] = CreateAgentTool()
	environments, err := loadAgentEnvironments(ctx, dataRoot, registry)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /tools", func(w http.ResponseWriter, r *http.Request) {
		names := make([]string, 0, len(registry))
		for name := range registry {
			names = append(names, name)
		}
		slices.Sort(names)
		tools := make([]ToolSummary, 0, len(names))
		for _, name := range names {
			tools = append(tools, ToolSummary{Name: name, Description: registry[name].Description})
		}
		writeJSONResponse(w, http.StatusOK, tools)
	})
	handleEnvironment := func(pattern string, handler func(http.ResponseWriter, *http.Request, *AgentEnvironment)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			env := environments[r.PathValue("id")]
			mu.Unlock()
			if env == nil {
				http.Error(w, "environment not found", http.StatusNotFound)
				return
			}
			handler(w, r, env)
		})
	}
	handleEnvironment("GET /environments/{id}/agents", func(w http.ResponseWriter, r *http.Request, env *AgentEnvironment) {
		env.mu.Lock()
		agents := make([]AgentSummary, 0, len(env.Agents))
		for _, agent := range env.Agents {
			agents = append(agents, AgentSummary{
				Name: agent.Name, Model: agent.ModelName, Instructions: agent.Instructions,
				Pending: len(agent.Inbox), Error: agent.InboxError,
				Tools:           append([]string{}, agent.ToolNames...),
				AssignableTools: append([]string{}, agent.AssignableToolNames...),
			})
		}
		env.mu.Unlock()
		writeJSONResponse(w, http.StatusOK, agents)
	})
	handleEnvironment("POST /environments/{id}/messages", func(w http.ResponseWriter, r *http.Request, env *AgentEnvironment) {
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		var envelope Envelope
		if err := decoder.Decode(&envelope); err != nil {
			http.Error(w, "invalid envelope JSON", http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			http.Error(w, "expected one envelope", http.StatusBadRequest)
			return
		}
		if envelope.Source == "agent" {
			http.Error(w, "agent source is reserved for the message tool", http.StatusBadRequest)
			return
		}
		id, err := env.Message(r.Context(), envelope)
		if err != nil {
			writeMessagingError(w, err)
			return
		}
		writeJSONResponse(w, http.StatusAccepted, map[string]string{"id": id, "status": "accepted"})
	})
	handleEnvironment("GET /environments/{id}/agents/{name}/inbox", func(w http.ResponseWriter, r *http.Request, env *AgentEnvironment) {
		inbox, err := env.Inbox(r.PathValue("name"))
		if err != nil {
			writeMessagingError(w, err)
			return
		}
		writeJSONResponse(w, http.StatusOK, inbox)
	})
	handleEnvironment("GET /environments/{id}/agents/{name}/session", func(w http.ResponseWriter, r *http.Request, env *AgentEnvironment) {
		env.mu.Lock()
		agent := env.agentLocked(r.PathValue("name"))
		if agent == nil {
			env.mu.Unlock()
			writeMessagingError(w, ErrAgentNotFound)
			return
		}
		snapshot := Session{SessionID: agent.Session.SessionID, Scope: agent.Session.Scope}
		env.mu.Unlock()
		if err := snapshot.Load(); err != nil {
			log.Printf("session snapshot failed: %v", err)
			http.Error(w, "cannot read session", http.StatusInternalServerError)
			return
		}
		writeJSONResponse(w, http.StatusOK, snapshot)
	})
	handleEnvironment("POST /environments/{id}/agents/{name}/inbox/retry", func(w http.ResponseWriter, r *http.Request, env *AgentEnvironment) {
		if err := env.RetryInbox(r.Context(), r.PathValue("name")); err != nil {
			writeMessagingError(w, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		handleEnvironment(method+" /environments/{id}/agents/{name}", func(w http.ResponseWriter, r *http.Request, env *AgentEnvironment) {
			name := r.PathValue("name")
			tools, err := resolveTools(registry, r.URL.Query()["tool"])
			if err != nil {
				writeMessagingError(w, err)
				return
			}
			assignableTools, err := resolveTools(registry, r.URL.Query()["assignable_tool"])
			if err != nil {
				writeMessagingError(w, err)
				return
			}
			if r.Method == http.MethodPut {
				err = env.UpdateAgent(r.Context(), name, r.URL.Query().Get("model"), r.URL.Query().Get("instructions"), tools, assignableTools...)
			} else {
				_, err = env.CreateAgent(ctx, r.URL.Query().Get("model"), tools, name, r.URL.Query().Get("instructions"), false, assignableTools...)
			}
			if err != nil {
				writeMessagingError(w, err)
				return
			}
			if r.Method == http.MethodPut {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			log.Printf("agent created: environment=%s name=%s", r.PathValue("id"), name)
			w.WriteHeader(http.StatusCreated)
		})
	}
	handleEnvironment("PUT /environments/{id}/agents/{name}/tools", func(w http.ResponseWriter, r *http.Request, env *AgentEnvironment) {
		var names []string
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
		if err := decoder.Decode(&names); err != nil || names == nil {
			http.Error(w, "expected a JSON array of tool names", http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			http.Error(w, "expected one JSON array", http.StatusBadRequest)
			return
		}
		tools, err := resolveTools(registry, names)
		if err == nil {
			err = env.SetAgentTools(r.Context(), r.PathValue("name"), tools)
		}
		if err != nil {
			writeMessagingError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /environments/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validStateName.MatchString(id) {
			http.Error(w, "invalid environment ID", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, exists := environments[id]; exists {
			http.Error(w, "environment already active", http.StatusConflict)
			return
		}
		env, err := NewAgentEnvironment(ctx, dataRoot, id)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				http.Error(w, "environment already exists", http.StatusConflict)
				return
			}
			log.Printf("environment creation failed: id=%s error=%v", id, err)
			http.Error(w, "cannot create environment", http.StatusInternalServerError)
			return
		}
		if err := env.Start(ctx); err != nil {
			http.Error(w, "cannot start environment", http.StatusServiceUnavailable)
			return
		}
		environments[id] = env
		log.Printf("environment created: id=%s data=%s", id, env.DataRoot)
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("GET /environments", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		names := make([]string, 0, len(environments))
		for name := range environments {
			names = append(names, name)
		}
		mu.Unlock()
		slices.Sort(names)
		writeJSONResponse(w, http.StatusOK, names)
	})
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return fmt.Errorf("restrict runtime socket permissions: %w", err)
	}
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, env := range environments {
			env.Close()
		}
	}()
	for _, env := range environments {
		if err := env.Start(ctx); err != nil {
			return fmt.Errorf("start environment %q: %w", env.Name, err)
		}
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	shutdownDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(shutdownDone)
		log.Printf("runtime shutdown requested")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown failed: %v", err)
			server.Close()
		}
	})
	defer func() {
		if !stop() {
			<-shutdownDone
		}
		server.Close()
	}()
	log.Printf("runtime listening: socket=%s data=%s", socketPath, dataRoot)
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		log.Printf("HTTP server stopped")
		return nil
	}
	return err
}

type AgentSummary struct {
	AssignableTools []string `json:"assignable_tools"`
	Name            string   `json:"name"`
	Model           string   `json:"model"`
	Instructions    string   `json:"instructions"`
	Pending         int      `json:"pending"`
	Error           string   `json:"error,omitempty"`
	Tools           []string `json:"tools"`
}

func resolveTools(registry ToolRegistry, names []string) ([]Tool, error) {
	tools := make([]Tool, 0, len(names))
	for _, name := range names {
		tool, ok := registry[name]
		if !ok {
			return nil, fmt.Errorf("%w %q", ErrInvalidTool, name)
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func writeJSONResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("JSON response failed: %v", err)
	}
}

func writeMessagingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrAgentNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, ErrAgentExists), errors.Is(err, ErrAgentBusy):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ErrInvalidEnvelope), errors.Is(err, ErrInvalidName), errors.Is(err, ErrModelNotFound), errors.Is(err, ErrInvalidTool):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrEnvironmentClosed), errors.Is(err, context.Canceled):
		http.Error(w, "environment is stopping", http.StatusServiceUnavailable)
	default:
		log.Printf("messaging operation failed: %v", err)
		http.Error(w, "messaging operation failed", http.StatusInternalServerError)
	}
}

func loadAgentEnvironments(ctx context.Context, dataRoot string, registry ToolRegistry) (map[string]*AgentEnvironment, error) {
	environments := make(map[string]*AgentEnvironment)
	entries, err := os.ReadDir(dataRoot)
	if errors.Is(err, os.ErrNotExist) {
		return environments, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read environments: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dataRoot, entry.Name(), "environment.json")); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("inspect environment %q: %w", entry.Name(), err)
		}
		env, err := LoadAgentEnvironment(ctx, dataRoot, entry.Name(), registry)
		if err != nil {
			return nil, fmt.Errorf("load environment %q: %w", entry.Name(), err)
		}
		environments[entry.Name()] = env
	}
	return environments, nil
}

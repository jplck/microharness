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
	registry := DefaultTools()
	environments, err := loadAgentEnvironments(ctx, dataRoot, registry)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /tools", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, http.StatusOK, registry.Optional())
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
				Status: agentStatusLocked(agent).Status, ActiveEnvelopeID: agent.activeEnvelopeID,
				Tools:           append([]string{}, agent.ToolNames...),
				AssignableTools: append([]string{}, agent.AssignableToolNames...),
				AllowedModels:   append([]string{}, agent.AllowedModels...),
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
		if envelope.Source == "agent" || envelope.Source == "runtime" {
			http.Error(w, "agent and runtime sources are reserved", http.StatusBadRequest)
			return
		}
		id, err := env.Message(r.Context(), envelope)
		if err != nil {
			writeMessagingError(w, err)
			return
		}
		delivery, err := env.Delivery(id)
		if err != nil {
			writeMessagingError(w, err)
			return
		}
		writeJSONResponse(w, http.StatusAccepted, map[string]string{"id": id, "status": "accepted", "delivery_status": delivery.Status})
	})
	handleEnvironment("GET /environments/{id}/messages", func(w http.ResponseWriter, r *http.Request, env *AgentEnvironment) {
		writeJSONResponse(w, http.StatusOK, env.Deliveries(r.URL.Query().Get("agent")))
	})
	handleEnvironment("GET /environments/{id}/messages/{message}", func(w http.ResponseWriter, r *http.Request, env *AgentEnvironment) {
		delivery, err := env.Delivery(r.PathValue("message"))
		if err != nil {
			writeMessagingError(w, err)
			return
		}
		writeJSONResponse(w, http.StatusOK, delivery)
	})
	handleEnvironment("POST /environments/{id}/messages/{message}/retract", func(w http.ResponseWriter, r *http.Request, env *AgentEnvironment) {
		var owner struct{ Source, Sender string }
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&owner); err != nil {
			http.Error(w, "expected message sender JSON", http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			http.Error(w, "expected one sender object", http.StatusBadRequest)
			return
		}
		if owner.Source == "" || owner.Sender == "" || owner.Source == "agent" || owner.Source == "runtime" {
			http.Error(w, "an external source and sender are required", http.StatusBadRequest)
			return
		}
		delivery, err := env.RetractMessage(r.Context(), owner.Source, owner.Sender, r.PathValue("message"))
		if err != nil {
			writeMessagingError(w, err)
			return
		}
		writeJSONResponse(w, http.StatusOK, delivery)
	})
	handleEnvironment("GET /environments/{id}/agents/{name}/status", func(w http.ResponseWriter, r *http.Request, env *AgentEnvironment) {
		status, err := env.Status(r.PathValue("name"))
		if err != nil {
			writeMessagingError(w, err)
			return
		}
		writeJSONResponse(w, http.StatusOK, status)
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
		err := snapshot.Load()
		env.mu.Unlock()
		if err != nil {
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
	handleEnvironment("DELETE /environments/{id}/agents/{name}", func(w http.ResponseWriter, r *http.Request, env *AgentEnvironment) {
		values := r.URL.Query()["erase_files"]
		if len(values) > 1 || len(values) == 1 && values[0] != "true" && values[0] != "false" {
			http.Error(w, "erase_files must be true or false", http.StatusBadRequest)
			return
		}
		eraseFiles := len(values) == 1 && values[0] == "true"
		if err := env.DeleteAgent(r.Context(), r.PathValue("name"), eraseFiles); err != nil {
			writeMessagingError(w, err)
			return
		}
		log.Printf("agent deleted: environment=%s name=%s erase_files=%t", r.PathValue("id"), r.PathValue("name"), eraseFiles)
		w.WriteHeader(http.StatusNoContent)
	})
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		handleEnvironment(method+" /environments/{id}/agents/{name}", func(w http.ResponseWriter, r *http.Request, env *AgentEnvironment) {
			name := r.PathValue("name")
			tools, err := registry.Resolve(r.URL.Query()["tool"])
			if err != nil {
				writeMessagingError(w, err)
				return
			}
			assignableTools, err := registry.Resolve(r.URL.Query()["assignable_tool"])
			if err != nil {
				writeMessagingError(w, err)
				return
			}
			if r.Method == http.MethodPut {
				err = env.UpdateAgentWithModels(r.Context(), name, r.URL.Query().Get("model"), r.URL.Query().Get("instructions"), tools, r.URL.Query()["allowed_model"], assignableTools...)
			} else {
				_, err = env.CreateAgentWithModels(ctx, r.URL.Query().Get("model"), tools, name, r.URL.Query().Get("instructions"), false, r.URL.Query()["allowed_model"], assignableTools...)
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
		tools, err := registry.Resolve(names)
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
		env.registry = registry
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
	Status           string   `json:"status"`
	ActiveEnvelopeID string   `json:"active_envelope_id,omitempty"`
	AllowedModels    []string `json:"allowed_models"`
	AssignableTools  []string `json:"assignable_tools"`
	Name             string   `json:"name"`
	Model            string   `json:"model"`
	Instructions     string   `json:"instructions"`
	Pending          int      `json:"pending"`
	Error            string   `json:"error,omitempty"`
	Tools            []string `json:"tools"`
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
	case errors.Is(err, ErrAgentNotFound), errors.Is(err, ErrMessageNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, ErrAgentExists), errors.Is(err, ErrAgentBusy), errors.Is(err, ErrAgentRunning), errors.Is(err, ErrMessageDelivered), errors.Is(err, ErrSteeringTarget), errors.Is(err, ErrPluginUpdateConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ErrMessageOwnership):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, ErrInvalidEnvelope), errors.Is(err, ErrInvalidName), errors.Is(err, ErrModelNotFound), errors.Is(err, ErrModelNotAllowed), errors.Is(err, ErrInvalidTool):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrEnvironmentClosed), errors.Is(err, context.Canceled):
		http.Error(w, "environment is stopping", http.StatusServiceUnavailable)
	case errors.Is(err, ErrAgentDataCleanup):
		log.Printf("agent file cleanup failed: %v", err)
		http.Error(w, ErrAgentDataCleanup.Error(), http.StatusInternalServerError)
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

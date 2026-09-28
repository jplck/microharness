package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	environments, err := loadAgentEnvironments(ctx, dataRoot, BuiltinTools())
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /environments/{id}/agents/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !validStateName.MatchString(name) {
			http.Error(w, "invalid agent name", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		env, exists := environments[r.PathValue("id")]
		if !exists {
			http.Error(w, "environment not found", http.StatusNotFound)
			return
		}
		if slices.ContainsFunc(env.Agents, func(agent *Agent) bool { return agent.Name == name }) {
			http.Error(w, "agent already exists", http.StatusConflict)
			return
		}
		config, err := GetModelByName(r.URL.Query().Get("model"))
		if err != nil {
			log.Printf("agent model lookup failed: %v", err)
			http.Error(w, "cannot resolve model", http.StatusBadRequest)
			return
		}
		_, err = env.CreateAgent(ctx, config.Name, nil, name, "", env.InitialAgent == nil)
		if err != nil {
			log.Printf("agent creation failed: name=%s error=%v", name, err)
			http.Error(w, "cannot create agent", http.StatusInternalServerError)
			return
		}
		log.Printf("agent created: environment=%s name=%s", r.PathValue("id"), name)
		w.WriteHeader(http.StatusCreated)
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
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(names); err != nil {
			log.Printf("environment list response failed: %v", err)
		}
	})
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
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
		mu.Lock()
		defer mu.Unlock()
		for _, env := range environments {
			env.Wait()
		}
	}()
	log.Printf("runtime listening: socket=%s data=%s", socketPath, dataRoot)
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		log.Printf("HTTP server stopped")
		return nil
	}
	return err
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

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"
)

func ServeRuntime(ctx context.Context, socketPath, dataRoot string) error {
	var mu sync.Mutex
	environments := make(map[string]*AgentEnvironment)
	validID := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /environments/{id}/agents/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !validID.MatchString(name) {
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
		client, err := NewModelClient(*config)
		if err != nil {
			http.Error(w, "cannot create model client", http.StatusInternalServerError)
			return
		}
		_, err = env.CreateAgent(ctx, client, nil, name, "", "", env.InitialAgent == nil)
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
		if !validID.MatchString(id) {
			http.Error(w, "invalid environment ID", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, exists := environments[id]; exists {
			http.Error(w, "environment already active", http.StatusConflict)
			return
		}
		directory := filepath.Join(dataRoot, id)
		store, err := LoadMemories(ctx, filepath.Join(directory, "memory.json"))
		if err != nil {
			log.Printf("environment creation failed: id=%s error=%v", id, err)
			http.Error(w, "cannot load memory", http.StatusInternalServerError)
			return
		}
		store.EmbeddingModel = "nomic-embed-text"
		environments[id] = &AgentEnvironment{MemoryStore: store, DataRoot: directory}
		log.Printf("environment created: id=%s data=%s", id, directory)
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("GET /environments", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		names := make([]string, 0, len(environments))
		for name := range environments {
			names = append(names, name)
		}
		mu.Unlock()
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
	stop := context.AfterFunc(ctx, func() {
		log.Printf("runtime shutdown requested")
		if err := server.Close(); err != nil {
			log.Printf("HTTP server close failed: %v", err)
		}
	})
	defer stop()
	log.Printf("runtime listening: socket=%s data=%s", socketPath, dataRoot)
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		log.Printf("HTTP server stopped")
		return nil
	}
	return err
}

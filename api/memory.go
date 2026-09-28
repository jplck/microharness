package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type Memory struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	Content        string    `json:"content"`
	SessionID      string    `json:"session_id,omitempty"`
	Approved       bool      `json:"approved,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
	EmbeddingModel string    `json:"embedding_model,omitempty"`
	Embedding      []float64 `json:"embedding,omitempty"`
}

type MemoryStore struct {
	Path           string
	entries        []Memory
	EmbeddingModel string
	mu             sync.RWMutex
}

func LoadMemories(path string) (*MemoryStore, error) {
	store := &MemoryStore{Path: path, entries: []Memory{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &store.entries); err != nil {
		return nil, err
	}
	return store, nil
}

func (store *MemoryStore) save() error {
	data, err := json.MarshalIndent(store.entries, "", "  ")
	if err != nil {
		return err
	}
	directory := filepath.Dir(store.Path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".memory-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), store.Path)
}

func (store *MemoryStore) Add(ctx context.Context, memory Memory) error {

	if store.EmbeddingModel != "" {
		model, err := GetModelByName(store.EmbeddingModel)
		if err != nil {
			return fmt.Errorf("get model by name: %w", err)
		}

		client, err := NewModelClient(*model)
		if err != nil {
			return fmt.Errorf("create model client: %w", err)
		}

		embedding, err := client.Embed(ctx, memory.Content)
		if err != nil {
			return fmt.Errorf("embed memory content: %w", err)
		}
		memory.Embedding = embedding
		memory.EmbeddingModel = store.EmbeddingModel
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	previous := store.entries

	memory.UpdatedAt = time.Now()
	store.entries = append(store.entries, memory)
	if err := store.save(); err != nil {
		store.entries = previous
		return err
	}
	return nil
}

func (store *MemoryStore) Search(query string) []Memory {
	store.mu.RLock()
	defer store.mu.RUnlock()

	query = strings.ToLower(strings.TrimSpace(query))
	matches := []Memory{}
	if query == "" {
		return matches
	}

	for index := len(store.entries) - 1; index >= 0; index-- {
		memory := store.entries[index]
		if strings.Contains(strings.ToLower(memory.Content), query) {
			memory.Embedding = slices.Clone(memory.Embedding)
			matches = append(matches, memory)
		}
	}
	return matches
}

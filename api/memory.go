package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type embeddingRun struct {
	cancel context.CancelFunc
}

type MemoryRevision struct {
	Revision  int       `json:"revision"`
	Kind      string    `json:"kind"`
	Content   string    `json:"content"`
	Approved  bool      `json:"approved"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Memory struct {
	ID             string           `json:"id"`
	Kind           string           `json:"kind"`
	Content        string           `json:"content"`
	SessionID      string           `json:"session_id,omitempty"`
	Approved       bool             `json:"approved,omitempty"`
	UpdatedAt      time.Time        `json:"updated_at"`
	EmbeddingModel string           `json:"embedding_model,omitempty"`
	Embedding      []float64        `json:"embedding,omitempty"`
	Revision       int              `json:"revision"`
	Revisions      []MemoryRevision `json:"revisions,omitempty"`
}

type MemoryStore struct {
	Path           string
	entries        []Memory
	EmbeddingModel string
	mu             sync.RWMutex
	embeddingRuns  map[string]*embeddingRun
	appCtx         context.Context
	embeddingWG    sync.WaitGroup
}

func LoadMemories(appCtx context.Context, path string) (*MemoryStore, error) {
	store := &MemoryStore{Path: path, entries: []Memory{}, appCtx: appCtx}
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

func (store *MemoryStore) queueEmbedding(appCtx context.Context, memory Memory) {
	store.mu.Lock()

	index := slices.IndexFunc(store.entries, func(entry Memory) bool {
		return entry.ID == memory.ID
	})
	if index < 0 {
		store.mu.Unlock()
		return
	}

	current := store.entries[index]
	if current.Content != memory.Content ||
		current.EmbeddingModel != memory.EmbeddingModel ||
		current.EmbeddingModel == "" ||
		len(current.Embedding) != 0 {
		store.mu.Unlock()
		return
	}

	if running := store.embeddingRuns[memory.ID]; running != nil {
		running.cancel()
		delete(store.embeddingRuns, memory.ID)
	}

	if store.embeddingRuns == nil {
		store.embeddingRuns = make(map[string]*embeddingRun)
	}
	ctx, cancel := context.WithTimeout(appCtx, time.Minute)
	run := &embeddingRun{cancel: cancel}
	store.embeddingRuns[memory.ID] = run
	store.embeddingWG.Add(1)
	store.mu.Unlock()

	go func() {
		defer store.embeddingWG.Done()
		defer cancel()
		embedding, err := store.embed(ctx, memory.EmbeddingModel, memory.Content)
		store.mu.Lock()
		defer store.mu.Unlock()
		if store.embeddingRuns[memory.ID] != run {
			return
		}
		delete(store.embeddingRuns, memory.ID)
		if err == nil {
			err = ctx.Err()
		}
		if err == nil && len(embedding) == 0 {
			err = fmt.Errorf("empty embedding returned")
		}
		if err != nil {
			log.Printf("embed memory %s: %v", memory.ID, err)
			return
		}
		index := slices.IndexFunc(store.entries, func(m Memory) bool { return m.ID == memory.ID })
		if index < 0 || store.entries[index].Content != memory.Content ||
			store.entries[index].EmbeddingModel != memory.EmbeddingModel {
			return
		}
		previous := store.entries[index]
		store.entries[index].Embedding = embedding
		if err := store.save(); err != nil {
			store.entries[index] = previous
			log.Printf("save embedding %s: %v", memory.ID, err)
		}
	}()
}

func (store *MemoryStore) update(ctx context.Context, changes Memory) (Memory, error) {
	if changes.ID == "" || strings.TrimSpace(changes.Content) == "" {
		return Memory{}, fmt.Errorf("ID and content are required")
	}
	switch changes.Kind {
	case "fact", "episode", "procedure":
	default:
		return Memory{}, fmt.Errorf("kind must be fact, episode, or procedure")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Memory{}, err
	}
	index := slices.IndexFunc(store.entries, func(entry Memory) bool { return entry.ID == changes.ID })
	if index < 0 {
		return Memory{}, fmt.Errorf("memory %q not found", changes.ID)
	}
	previous := store.entries[index]
	if previous.Revision != changes.Revision {
		return Memory{}, fmt.Errorf("memory %q has changed; reload before updating", changes.ID)
	}

	next := previous
	next.Kind, next.Content, next.Approved = changes.Kind, changes.Content, changes.Approved
	contentChanged := next.Content != previous.Content
	kindChanged := next.Kind != previous.Kind
	if contentChanged || kindChanged {
		next.Approved = false
	}
	if contentChanged {
		next.Embedding = nil
		next.EmbeddingModel = store.EmbeddingModel
	}
	if contentChanged || kindChanged || next.Approved != previous.Approved {
		next.Revision = previous.Revision + 1
		next.UpdatedAt = time.Now()
		next.Revisions = append(slices.Clone(previous.Revisions), MemoryRevision{
			Revision: previous.Revision, Kind: previous.Kind, Content: previous.Content,
			Approved: previous.Approved, UpdatedAt: previous.UpdatedAt,
		})
		store.entries[index] = next
		if err := store.save(); err != nil {
			store.entries[index] = previous
			return Memory{}, err
		}
	}
	next.Embedding = slices.Clone(next.Embedding)
	next.Revisions = slices.Clone(next.Revisions)
	return next, nil
}

func (store *MemoryStore) Update(ctx context.Context, changes Memory) (Memory, error) {
	saved, err := store.update(ctx, changes)
	if err != nil {
		return Memory{}, err
	}

	store.queueEmbedding(store.appCtx, saved)
	return saved, nil
}

func (store *MemoryStore) embed(ctx context.Context, modelName, input string) ([]float64, error) {
	config, err := GetModelByName(modelName)
	if err != nil {
		return nil, err
	}
	client, err := NewModelClient(*config)
	if err != nil {
		return nil, err
	}
	embedding, err := client.Embed(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("embed memory content: %w", err)
	}
	return embedding, nil
}

func (store *MemoryStore) Add(ctx context.Context, memory Memory) error {
	saved, err := store.add(ctx, memory)
	if err != nil {
		return err
	}

	store.queueEmbedding(store.appCtx, saved)
	return nil
}

func (store *MemoryStore) add(ctx context.Context, memory Memory) (Memory, error) {
	store.mu.Lock()
	defer store.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return Memory{}, err
	}

	memory.ID = fmt.Sprintf("%d", time.Now().UnixNano())
	memory.UpdatedAt = time.Now()
	memory.Revision = 1
	memory.Revisions = nil
	memory.Embedding = nil
	memory.EmbeddingModel = store.EmbeddingModel

	previous := store.entries
	store.entries = append(store.entries, memory)
	if err := store.save(); err != nil {
		store.entries = previous
		return Memory{}, err
	}
	return memory, nil
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
			memory.Revisions = slices.Clone(memory.Revisions)
			matches = append(matches, memory)
		}
	}
	return matches
}

func (store *MemoryStore) Wait() {
	store.embeddingWG.Wait()
}

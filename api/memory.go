package api

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Memory struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Content   string    `json:"content"`
	SessionID string    `json:"session_id,omitempty"`
	Approved  bool      `json:"approved,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type MemoryStore struct {
	Path    string
	Entries []Memory
}

func LoadMemories(path string) (*MemoryStore, error) {
	store := &MemoryStore{Path: path, Entries: []Memory{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &store.Entries); err != nil {
		return nil, err
	}
	return store, nil
}

func (store *MemoryStore) save() error {
	data, err := json.MarshalIndent(store.Entries, "", "  ")
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

func (store *MemoryStore) Add(memory Memory) error {
	memory.UpdatedAt = time.Now()
	store.Entries = append(store.Entries, memory)
	if err := store.save(); err != nil {
		return err
	}
	return nil
}

func (store *MemoryStore) Search(query string) []Memory {
	query = strings.ToLower(strings.TrimSpace(query))
	matches := []Memory{}
	if query == "" {
		return matches
	}

	for index := len(store.Entries) - 1; index >= 0; index-- {
		memory := store.Entries[index]
		if strings.Contains(strings.ToLower(memory.Content), query) {
			matches = append(matches, memory)
		}
	}
	return matches
}

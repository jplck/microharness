package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Session struct {
	SessionID string
	Messages  []Message
	Scope     string
}

func (s *Session) persist() error {

	if s.SessionID == "" || s.SessionID == "." || s.SessionID == ".." ||
		filepath.Base(s.SessionID) != s.SessionID {
		return fmt.Errorf("session ID must be a non-empty filename")
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}

	filename := filepath.Join(s.Scope, s.SessionID+".json")
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}

	return os.WriteFile(filename, data, 0o600)
}

func (s *Session) Load() error {
	if s.SessionID == "" || s.SessionID == "." || s.SessionID == ".." ||
		filepath.Base(s.SessionID) != s.SessionID {
		return fmt.Errorf("session ID must be a non-empty filename")
	}

	filename := filepath.Join(s.Scope, s.SessionID+".json")
	data, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("read session file: %w", err)
	}

	if err := json.Unmarshal(data, s); err != nil {
		return fmt.Errorf("decode session: %w", err)
	}

	return nil
}

func (s *Session) AddMessage(msg Message) error {
	s.Messages = append(s.Messages, msg)
	if err := s.persist(); err != nil {
		fmt.Printf("failed to persist session: %v\n", err)
		return err
	}
	return nil
}

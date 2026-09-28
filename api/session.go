package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Session struct {
	SessionID string    `json:"-"`
	Messages  []Message `json:"messages"`
	Scope     string    `json:"-"`
}

func (s *Session) path() (string, error) {
	if s.SessionID == "" || s.SessionID == "." || s.SessionID == ".." ||
		filepath.Base(s.SessionID) != s.SessionID {
		return "", fmt.Errorf("session ID must be a non-empty filename")
	}
	return filepath.Join(s.Scope, s.SessionID+".json"), nil
}

func (s *Session) persist() error {
	filename, err := s.path()
	if err != nil {
		return err
	}
	return writeJSONAtomic(filename, s)
}

func (s *Session) Load() error {
	filename, err := s.path()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("read session file: %w", err)
	}

	var saved Session
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("decode session: %w", err)
	}
	s.Messages = saved.Messages

	return nil
}

func (s *Session) AddMessage(msg Message) error {
	s.Messages = append(s.Messages, msg)
	if err := s.persist(); err != nil {
		s.Messages = s.Messages[:len(s.Messages)-1]
		return err
	}
	return nil
}

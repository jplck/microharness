package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Session struct {
	SessionID string
	Messages  []Message
}

func (s *Session) Persist() error {
	if s.SessionID == "" ||
		filepath.Base(s.SessionID) != s.SessionID {
		return fmt.Errorf("session ID must be a non-empty filename")
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}

	filename := s.SessionID + ".json"
	return os.WriteFile(filename, data, 0o600)
}

func (s *Session) Load() error {
	if s.SessionID == "" ||
		filepath.Base(s.SessionID) != s.SessionID {
		return fmt.Errorf("session ID must be a non-empty filename")
	}

	filename := s.SessionID + ".json"
	data, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("read session file: %w", err)
	}

	if err := json.Unmarshal(data, s); err != nil {
		return fmt.Errorf("decode session: %w", err)
	}

	return nil
}

func (s *Session) AddMessage(msg Message) {
	s.Messages = append(s.Messages, msg)
}

type AgentEnvironment struct {
	Agents []Agent
}

type Agent struct {
	Client       ModelCall
	Tools        []Tool
	Name         string
	Instructions string
	Session      Session
}

func CreateAgent(ctx context.Context, client ModelCall, tools []Tool, name string, instructions string, sessionID string) (*Agent, error) {

	var session Session
	if sessionID == "" {
		session = Session{SessionID: generateSessionID()}
		session.AddMessage(Message{Role: "system", Content: instructions})
		if err := session.Persist(); err != nil {
			return nil, fmt.Errorf("persist session: %w", err)
		}
	} else {
		session = Session{SessionID: sessionID}
		if err := session.Load(); err != nil {
			return nil, fmt.Errorf("load session: %w", err)
		}
	}

	return &Agent{
		Client:       client,
		Tools:        tools,
		Name:         name,
		Instructions: instructions,
		Session:      session,
	}, nil
}

func generateSessionID() string {
	return rand.Text()
}

func (a *Agent) Execute(ctx context.Context, input Message) (Message, error) {
	tools := make(map[string]Tool, len(a.Tools))
	for _, tool := range a.Tools {
		tools[tool.Name] = tool
	}

	a.Session.AddMessage(input)
	if err := a.Session.Persist(); err != nil {
		return Message{}, fmt.Errorf("persist session: %w", err)
	}

	for step := 0; step < 10; step++ {

		resp, err := a.Client.Call(ctx, a.Session.Messages, a.Tools)
		if err != nil {
			return Message{}, fmt.Errorf("call client: %w", err)
		}

		a.Session.AddMessage(resp)

		if len(resp.ToolCalls) > 0 {
			for _, call := range resp.ToolCalls {
				if tool, ok := tools[call.Name]; ok {
					if toolOutput, err := tool.Execute(ctx, call.Arguments); err != nil {
						return Message{}, fmt.Errorf("execute tool %s: %w", call.Name, err)
					} else {
						a.Session.AddMessage(Message{Role: "tool", Content: toolOutput, ToolCallID: call.ID})
					}
				} else {
					return Message{}, fmt.Errorf("tool %s not found", call.Name)
				}
			}
		}

		if err := a.Session.Persist(); err != nil {
			return Message{}, fmt.Errorf("persist session: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return Message{}, err
		}
		if len(resp.ToolCalls) == 0 {
			return resp, nil
		}
	}
	return Message{}, fmt.Errorf("agent exceeded 10 model calls")
}

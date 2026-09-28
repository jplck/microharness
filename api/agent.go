package api

import (
	"context"
	"crypto/rand"
	"fmt"
)

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

	session := Session{SessionID: sessionID, Scope: "Sessions/"}

	if sessionID == "" {
		session.SessionID = generateSessionID()
		session.AddMessage(Message{Role: "system", Content: instructions})
		if err := session.Persist(); err != nil {
			return nil, fmt.Errorf("persist session: %w", err)
		}
	} else if err := session.Load(); err != nil {
		return nil, fmt.Errorf("load session: %w", err)
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

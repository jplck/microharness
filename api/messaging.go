package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
)

var ErrAgentNotFound = errors.New("agent not found")
var ErrAgentExists = errors.New("agent already exists")
var ErrInvalidName = errors.New("invalid name")
var ErrInvalidEnvelope = errors.New("invalid envelope")
var ErrEnvironmentClosed = errors.New("environment is closed")

type envelopeContextKey struct{}

type InboxState struct {
	Messages         []Envelope `json:"messages"`
	Error            string     `json:"error,omitempty"`
	Status           string     `json:"status"`
	ActiveEnvelopeID string     `json:"active_envelope_id,omitempty"`
}

type AgentStatus struct {
	Name             string `json:"name"`
	Status           string `json:"status"`
	Pending          int    `json:"pending"`
	ActiveEnvelopeID string `json:"active_envelope_id,omitempty"`
	Error            string `json:"error,omitempty"`
}

func agentStatusLocked(agent *Agent) AgentStatus {
	state := AgentStatus{Name: agent.Name, Status: "idle", Pending: len(agent.Inbox), ActiveEnvelopeID: agent.activeEnvelopeID, Error: agent.InboxError}
	switch {
	case agent.InboxError != "":
		state.Status = "paused"
	case agent.activeEnvelopeID != "":
		state.Status = "running"
	case len(agent.Inbox) > 0:
		state.Status = "queued"
	}
	return state
}

func (env *AgentEnvironment) Status(name string) (AgentStatus, error) {
	env.mu.Lock()
	defer env.mu.Unlock()
	agent := env.agentLocked(name)
	if agent == nil {
		return AgentStatus{}, fmt.Errorf("%w: %q", ErrAgentNotFound, name)
	}
	return agentStatusLocked(agent), nil
}

func (env *AgentEnvironment) saveFailureNotificationsLocked(agent *Agent) error {
	previousNotified := agent.notifiedFailures
	previousLengths := make(map[*Agent]int)
	for _, request := range agent.Inbox {
		if request.Source != "agent" || slices.Contains(agent.notifiedFailures, request.ID) {
			continue
		}
		receiver := env.agentLocked(request.Sender)
		if receiver == nil {
			continue
		}
		if _, exists := previousLengths[receiver]; !exists {
			previousLengths[receiver] = len(receiver.Inbox)
		}
		content, _ := json.Marshal(map[string]string{
			"event": "agent_blocked", "agent": agent.Name, "status": "paused",
			"envelope_id": request.ID, "failed_envelope_id": agent.Inbox[0].ID, "error": agent.InboxError,
		})
		receiver.Inbox = append(receiver.Inbox, Envelope{ID: rand.Text(), Source: "runtime", Sender: "runtime", To: receiver.Name, ConversationID: request.ConversationID, ReplyTo: request.ID, Content: string(content)})
		agent.notifiedFailures = append(agent.notifiedFailures, request.ID)
	}
	if err := env.saveLocked(); err != nil {
		for receiver, length := range previousLengths {
			receiver.Inbox = receiver.Inbox[:length]
		}
		agent.notifiedFailures = previousNotified
		return err
	}
	for receiver := range previousLengths {
		env.notifyLocked(receiver)
	}
	return nil
}

func (env *AgentEnvironment) agentLocked(name string) *Agent {
	for _, agent := range env.Agents {
		if agent.Name == name {
			return agent
		}
	}
	return nil
}

func (env *AgentEnvironment) Message(ctx context.Context, envelope Envelope) (string, error) {
	env.mu.Lock()
	defer env.mu.Unlock()
	if env.closed {
		return "", ErrEnvironmentClosed
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(envelope.Content) == "" || strings.TrimSpace(envelope.Sender) == "" {
		return "", fmt.Errorf("%w: sender and content are required", ErrInvalidEnvelope)
	}
	if envelope.Source == "" {
		envelope.Source = "external"
	}
	if envelope.Source == "agent" && env.agentLocked(envelope.Sender) == nil {
		return "", fmt.Errorf("%w: sender %q", ErrAgentNotFound, envelope.Sender)
	}
	if envelope.To == "" && env.InitialAgent != nil {
		envelope.To = env.InitialAgent.Name
	}
	recipient := env.agentLocked(envelope.To)
	if recipient == nil {
		return "", fmt.Errorf("%w: recipient %q", ErrAgentNotFound, envelope.To)
	}
	if envelope.ID == "" {
		envelope.ID = rand.Text()
	}
	if envelope.ConversationID == "" {
		envelope.ConversationID = envelope.ID
	}
	for _, agent := range env.Agents {
		for _, queued := range agent.Inbox {
			if queued.ID == envelope.ID {
				if queued == envelope {
					return envelope.ID, nil
				}
				return "", fmt.Errorf("%w: message ID %q already has different content", ErrInvalidEnvelope, envelope.ID)
			}
		}
	}
	recipient.Inbox = append(recipient.Inbox, envelope)
	var err error
	if recipient.InboxError != "" {
		err = env.saveFailureNotificationsLocked(recipient)
	} else {
		err = env.saveLocked()
	}
	if err != nil {
		recipient.Inbox = recipient.Inbox[:len(recipient.Inbox)-1]
		return "", err
	}
	env.notifyLocked(recipient)
	return envelope.ID, nil
}

func (env *AgentEnvironment) Inbox(name string) (InboxState, error) {
	env.mu.Lock()
	defer env.mu.Unlock()
	agent := env.agentLocked(name)
	if agent == nil {
		return InboxState{}, fmt.Errorf("%w: %q", ErrAgentNotFound, name)
	}
	return InboxState{Messages: append([]Envelope{}, agent.Inbox...), Error: agent.InboxError, Status: agentStatusLocked(agent).Status, ActiveEnvelopeID: agent.activeEnvelopeID}, nil
}

func (env *AgentEnvironment) Start(ctx context.Context) error {
	env.mu.Lock()
	defer env.mu.Unlock()
	if env.closed {
		return ErrEnvironmentClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if env.workerCtx != nil {
		return nil
	}
	for _, agent := range env.Agents {
		if agent.InboxError != "" {
			if err := env.saveFailureNotificationsLocked(agent); err != nil {
				return err
			}
		}
	}
	env.workerCtx, env.workerCancel = context.WithCancel(ctx)
	for _, agent := range env.Agents {
		env.startWorkerLocked(agent)
	}
	return nil
}

func (env *AgentEnvironment) Close() {
	env.mu.Lock()
	env.closed = true
	if env.workerCancel != nil {
		env.workerCancel()
	}
	env.mu.Unlock()
	env.workerWG.Wait()
	env.MemoryStore.Wait()
}

func (env *AgentEnvironment) startWorkerLocked(agent *Agent) {
	agent.inboxWake = make(chan struct{}, 1)
	env.workerWG.Add(1)
	go env.runInbox(env.workerCtx, agent)
	env.notifyLocked(agent)
}

func (env *AgentEnvironment) notifyLocked(agent *Agent) {
	select {
	case agent.inboxWake <- struct{}{}:
	default:
	}
}

func (env *AgentEnvironment) runInbox(ctx context.Context, agent *Agent) {
	defer env.workerWG.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-agent.inboxWake:
		}
		for {
			env.mu.Lock()
			if ctx.Err() != nil || len(agent.Inbox) == 0 || agent.InboxError != "" {
				env.mu.Unlock()
				break
			}
			envelope := agent.Inbox[0]
			agent.activeEnvelopeID = envelope.ID
			env.mu.Unlock()
			err := agent.executeEnvelope(ctx, envelope)
			env.mu.Lock()
			agent.activeEnvelopeID = ""
			if ctx.Err() != nil {
				env.mu.Unlock()
				return
			}
			if err == nil {
				previous := agent.Inbox
				agent.Inbox = agent.Inbox[1:]
				if err = env.saveLocked(); err != nil {
					agent.Inbox = previous
				}
			}
			if err != nil {
				agent.InboxError = err.Error()
				log.Printf("agent inbox paused: environment=%s agent=%s message=%s error=%v", env.Name, agent.Name, envelope.ID, err)
				if saveErr := env.saveFailureNotificationsLocked(agent); saveErr != nil {
					log.Printf("save inbox failure: %v", saveErr)
				}
			}
			env.mu.Unlock()
			if err != nil {
				break
			}
		}
	}
}

func (env *AgentEnvironment) RetryInbox(ctx context.Context, name string) error {
	env.mu.Lock()
	defer env.mu.Unlock()
	if env.closed {
		return ErrEnvironmentClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	agent := env.agentLocked(name)
	if agent == nil {
		return fmt.Errorf("%w: %q", ErrAgentNotFound, name)
	}
	previous := agent.InboxError
	previousNotified := agent.notifiedFailures
	agent.InboxError = ""
	agent.notifiedFailures = nil
	if err := env.saveLocked(); err != nil {
		agent.InboxError = previous
		agent.notifiedFailures = previousNotified
		return err
	}
	env.notifyLocked(agent)
	return nil
}

func (env *AgentEnvironment) messagingTools(sender string) []Tool {
	return []Tool{
		{
			Name:        "agent_status",
			Description: "Inspect an agent's idle, queued, running, or paused status, pending count, active envelope ID, and error. Check this before sending reminders. A paused inbox requires an operator retry; more messages do not resume it.",
			Parameters:  []Parameter{{Name: "name", Type: String, Description: "Agent name to inspect", Required: true}},
			Execute: JSONHandler(func(ctx context.Context, arguments struct {
				Name string `json:"name"`
			}) (string, error) { status, err := env.Status(arguments.Name); if err != nil {
				return "", err
			}; data, err := json.Marshal(status); return string(data), err }),
		},
		{
			Name: "list_agents", Description: "List agent names in this environment that can receive messages.",
			Execute: JSONHandler(func(ctx context.Context, arguments struct{}) (string, error) {
				env.mu.Lock()
				defer env.mu.Unlock()
				names := make([]string, 0, len(env.Agents))
				for _, agent := range env.Agents {
					names = append(names, agent.Name)
				}
				slices.Sort(names)
				data, err := json.Marshal(names)
				return string(data), err
			}),
		},
		{
			Name:        "message",
			Description: "Queue a message to another existing agent and return its accepted ID plus a recipient status snapshot, not an answer or task completion. A paused recipient cannot process it until an operator retries its inbox. Use agent_status before sending reminders. Replies and runtime failure notices arrive as separate inbox turns. When Source is \"agent\", send substantive replies to the incoming envelope's Sender and set reply_to to its ID. When Source is not \"agent\", reply with normal assistant text; client labels such as \"tui\" or \"user\" and the runtime are not agent recipients. You may still delegate work to existing agents. Do not send acknowledgement-only replies or wait for another agent in this turn.",
			Parameters: []Parameter{
				{Name: "to", Type: String, Description: "Recipient agent name", Required: true},
				{Name: "content", Type: String, Description: "Message content", Required: true},
				{Name: "reply_to", Type: String, Description: "Envelope ID being answered, or empty for a new message"},
			},
			Execute: JSONHandler(func(ctx context.Context, arguments struct {
				To      string `json:"to"`
				Content string `json:"content"`
				ReplyTo string `json:"reply_to"`
			}) (string, error) {
				if arguments.To == "" {
					return "", fmt.Errorf("%w: recipient is required", ErrInvalidEnvelope)
				}
				incoming, _ := ctx.Value(envelopeContextKey{}).(Envelope)
				id, err := env.Message(ctx, Envelope{
					Source: "agent", Sender: sender, To: arguments.To, Content: arguments.Content,
					ReplyTo: arguments.ReplyTo, ConversationID: incoming.ConversationID,
				})
				if err != nil {
					return "", err
				}
				status, err := env.Status(arguments.To)
				if err != nil {
					return "", err
				}
				data, err := json.Marshal(struct {
					ID        string      `json:"id"`
					Status    string      `json:"status"`
					Recipient AgentStatus `json:"recipient"`
				}{id, "accepted", status})
				return string(data), err
			}),
		},
	}
}

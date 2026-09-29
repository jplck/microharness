package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type messageTestModel struct {
	call func(context.Context, []Message) (Message, error)
}

func (model messageTestModel) Call(ctx context.Context, messages []Message, tools []Tool) (Message, error) {
	return model.call(ctx, messages)
}

func (model messageTestModel) Embed(ctx context.Context, input string) ([]float64, error) {
	return nil, errors.New("unexpected embedding call")
}

func waitForInbox(t *testing.T, env *AgentEnvironment, name string, ready func([]Envelope, string) bool) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		env.mu.Lock()
		agent := env.agentLocked(name)
		matched := ready(agent.Inbox, agent.InboxError)
		env.mu.Unlock()
		if matched {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("inbox condition not met for %q", name)
		}
	}
}

func TestAgentCreatesAndMessagesChild(t *testing.T) {
	ctx, root, _ := setupPersistenceTest(t)
	called := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"child-response","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Task completed"}}]}`)
	}))
	defer server.Close()
	if err := writeJSONAtomic("models.json", map[string]any{"models": []Model{{Name: "test-model", Provider: ProviderOpenAI, Endpoint: server.URL}}}); err != nil {
		t.Fatal(err)
	}
	env, err := NewAgentEnvironment(ctx, root, "team")
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	parent, err := env.CreateAgent(ctx, "test-model", []Tool{CreateAgentTool()}, "parent", "Delegate tasks", true)
	if err != nil {
		t.Fatal(err)
	}
	parent.Client = messageTestModel{call: func(ctx context.Context, messages []Message) (Message, error) {
		if messages[len(messages)-1].Role == "user" {
			return Message{Role: "assistant", ToolCalls: []ToolCall{
				{ID: "create-child", Name: "create_agent", Arguments: json.RawMessage(`{"name":"child","model":"test-model","instructions":"Complete assigned tasks"}`)},
				{ID: "delegate", Name: "message", Arguments: json.RawMessage(`{"to":"child","content":"Complete this task"}`)},
			}}, nil
		}
		return Message{Role: "assistant", Content: "Delegated"}, nil
	}}
	if err := env.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Message(ctx, Envelope{Sender: "user", To: "parent", Content: "Create a helper and delegate", ConversationID: "task-1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("tool-created child did not process delegated work")
	}
	for _, name := range []string{"parent", "child"} {
		waitForInbox(t, env, name, func(inbox []Envelope, failure string) bool { return len(inbox) == 0 && failure == "" })
	}
	env.Close()
	child := env.agentLocked("child")
	if child == nil || len(child.ToolNames) != 0 || child.Session.Messages[len(child.Session.Messages)-1].Content != "Task completed" {
		t.Fatal("child did not complete and persist its task")
	}
}

func TestAgentsMessageWithoutWaitingForReplies(t *testing.T) {
	ctx, root, _ := setupPersistenceTest(t)
	env, err := NewAgentEnvironment(ctx, root, "mail")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(env.Close)
	sender, err := env.CreateAgent(ctx, "test-model", nil, "sender", "", true)
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := env.CreateAgent(ctx, "test-model", nil, "recipient", "", false)
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan Envelope, 1)
	replies := make(chan Envelope, 1)
	release := make(chan struct{})
	sender.Client = messageTestModel{call: func(ctx context.Context, messages []Message) (Message, error) {
		incoming := ctx.Value(envelopeContextKey{}).(Envelope)
		if incoming.Source == "agent" {
			replies <- incoming
			return Message{Role: "assistant", Content: "Received the result"}, nil
		}
		if messages[len(messages)-1].Role == "user" {
			return Message{Role: "assistant", ToolCalls: []ToolCall{{
				ID: "send-request", Name: "message", Arguments: json.RawMessage(`{"to":"recipient","content":"Check this","sender":"spoofed"}`),
			}}}, nil
		}
		return Message{Role: "assistant", Content: "Message queued"}, nil
	}}
	recipient.Client = messageTestModel{call: func(ctx context.Context, messages []Message) (Message, error) {
		incoming := ctx.Value(envelopeContextKey{}).(Envelope)
		if messages[len(messages)-1].Role == "user" {
			received <- incoming
			select {
			case <-release:
			case <-ctx.Done():
				return Message{}, ctx.Err()
			}
			arguments, _ := json.Marshal(map[string]string{"to": incoming.Sender, "content": "Checked", "reply_to": incoming.ID})
			return Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "send-reply", Name: "message", Arguments: arguments}}}, nil
		}
		return Message{Role: "assistant", Content: "Done"}, nil
	}}
	if _, err := env.Message(ctx, Envelope{Sender: "user", To: "sender", Content: "Start", ConversationID: "task-1"}); err != nil {
		t.Fatal(err)
	}
	if err := env.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var request Envelope
	select {
	case request = <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("recipient was not invoked")
	}
	if request.Sender != "sender" || request.Source != "agent" || request.ConversationID != "task-1" {
		t.Fatalf("invalid routed metadata: %+v", request)
	}
	waitForInbox(t, env, "sender", func(inbox []Envelope, failure string) bool { return len(inbox) == 0 && failure == "" })
	close(release)
	select {
	case reply := <-replies:
		if reply.Sender != "recipient" || reply.ReplyTo != request.ID || reply.ConversationID != "task-1" {
			t.Fatalf("invalid reply metadata: %+v", reply)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reply was not delivered in a new sender turn")
	}
	for _, name := range []string{"sender", "recipient"} {
		waitForInbox(t, env, name, func(inbox []Envelope, failure string) bool { return len(inbox) == 0 && failure == "" })
	}
}

func TestInboxResumesAfterShutdownAndFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint("failure=", failure), func(t *testing.T) {
			ctx, root, registry := setupPersistenceTest(t)
			env, err := NewAgentEnvironment(ctx, root, "mail")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(env.Close)
			agent, err := env.CreateAgent(ctx, "test-model", nil, "recipient", "", true)
			if err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			agent.Client = messageTestModel{call: func(ctx context.Context, messages []Message) (Message, error) {
				close(entered)
				if failure {
					return Message{}, errors.New("model unavailable")
				}
				<-ctx.Done()
				return Message{}, ctx.Err()
			}}
			id, err := env.Message(ctx, Envelope{Sender: "user", Content: "Queued work"})
			if err != nil {
				t.Fatal(err)
			}
			if err := env.Start(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not start")
			}
			if failure {
				waitForInbox(t, env, "recipient", func(inbox []Envelope, failure string) bool { return len(inbox) == 1 && failure != "" })
			}
			env.Close()
			loaded, err := LoadAgentEnvironment(ctx, root, "mail", registry)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(loaded.Close)
			if len(loaded.InitialAgent.Inbox) != 1 || (loaded.InitialAgent.InboxError != "") != failure {
				t.Fatal("pending message or failure state was lost")
			}
			loaded.InitialAgent.Client = messageTestModel{call: func(ctx context.Context, messages []Message) (Message, error) {
				return Message{Role: "assistant", Content: "Completed after restart"}, nil
			}}
			if err := loaded.Start(ctx); err != nil {
				t.Fatal(err)
			}
			if failure {
				if err := loaded.RetryInbox(ctx, "recipient"); err != nil {
					t.Fatal(err)
				}
			}
			waitForInbox(t, loaded, "recipient", func(inbox []Envelope, failure string) bool { return len(inbox) == 0 && failure == "" })
			loaded.Close()
			inputs := 0
			for _, message := range loaded.InitialAgent.Session.Messages {
				if message.EnvelopeID == id {
					inputs++
				}
			}
			if inputs != 1 {
				t.Fatalf("retry duplicated the session input: %d", inputs)
			}
		})
	}
}

func TestInboxRecognizesSavedProgress(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprint("completed=", completed), func(t *testing.T) {
			ctx, root, _ := setupPersistenceTest(t)
			env, err := NewAgentEnvironment(ctx, root, "mail")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(env.Close)
			toolCalls, modelCalls := 0, 0
			tool := Tool{Name: "count", Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
				toolCalls++
				return "done", nil
			}}
			agent, err := env.CreateAgent(ctx, "test-model", []Tool{tool}, "recipient", "", true)
			if err != nil {
				t.Fatal(err)
			}
			id, err := env.Message(ctx, Envelope{Sender: "user", Content: "Pending acknowledgement"})
			if err != nil {
				t.Fatal(err)
			}
			progress := []Message{{Role: "user", EnvelopeID: id, Content: "Saved input"}}
			if completed {
				progress = append(progress, Message{Role: "assistant", Content: "Already completed"})
			} else {
				progress = append(progress,
					Message{Role: "assistant", ToolCalls: []ToolCall{
						{ID: "completed-tool", Name: "count", Arguments: json.RawMessage(`{}`)},
						{ID: "pending-tool", Name: "count", Arguments: json.RawMessage(`{}`)},
					}},
					Message{Role: "tool", ToolCallID: "completed-tool", Content: "Already saved"},
				)
			}
			for _, message := range progress {
				if err := agent.Session.AddMessage(message); err != nil {
					t.Fatal(err)
				}
			}
			agent.Client = messageTestModel{call: func(ctx context.Context, messages []Message) (Message, error) {
				modelCalls++
				return Message{Role: "assistant", Content: "Finished remaining work"}, nil
			}}
			if err := env.Start(ctx); err != nil {
				t.Fatal(err)
			}
			if err := env.Start(ctx); err != nil {
				t.Fatal(err)
			}
			waitForInbox(t, env, "recipient", func(inbox []Envelope, failure string) bool { return len(inbox) == 0 && failure == "" })
			env.Close()
			wantCalls := 1
			if completed {
				wantCalls = 0
			}
			if modelCalls != wantCalls || toolCalls != wantCalls {
				t.Fatalf("saved work was replayed: model=%d tool=%d, want %d", modelCalls, toolCalls, wantCalls)
			}
		})
	}
}

func TestMessageDeliveryPersists(t *testing.T) {
	ctx, root, registry := setupPersistenceTest(t)
	env, err := NewAgentEnvironment(ctx, root, "mail")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sender", "recipient"} {
		if _, err := env.CreateAgent(ctx, "test-model", nil, name, "", name == "recipient"); err != nil {
			t.Fatal(err)
		}
	}
	envelope := Envelope{Source: "agent", Sender: "sender", Content: "Please check this", ConversationID: "thread-1", ReplyTo: "request-1"}
	id, err := env.Message(ctx, envelope)
	if err != nil || id == "" {
		t.Fatalf("enqueue: %q, %v", id, err)
	}
	loaded, err := LoadAgentEnvironment(ctx, root, "mail", registry)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := loaded.Inbox("recipient")
	if err != nil || len(inbox.Messages) != 1 {
		t.Fatalf("restored inbox: %v, %v", inbox, err)
	}
	envelope.ID, envelope.To = id, "recipient"
	if inbox.Messages[0] != envelope {
		t.Fatalf("envelope changed: %+v", inbox.Messages[0])
	}
	if _, err := loaded.Message(ctx, envelope); err != nil {
		t.Fatal(err)
	}
	inbox, _ = loaded.Inbox("recipient")
	if len(inbox.Messages) != 1 {
		t.Fatal("pending message delivered twice")
	}
	envelope.Content = "different"
	if _, err := loaded.Message(ctx, envelope); !errors.Is(err, ErrInvalidEnvelope) {
		t.Fatalf("conflicting duplicate accepted: %v", err)
	}
	envelope.ID, envelope.To = "", "missing"
	if _, err := loaded.Message(ctx, envelope); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("missing recipient accepted: %v", err)
	}
	envelope.To = "recipient"
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := loaded.Message(cancelled, envelope); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled delivery accepted: %v", err)
	}
	filename := filepath.Join(loaded.DataRoot, "environment.json")
	if err := os.Rename(filename, filename+".backup"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filename, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := loaded.Message(ctx, envelope); err == nil {
		t.Fatal("delivery acknowledged despite persistence failure")
	}
	inbox, _ = loaded.Inbox("recipient")
	if len(inbox.Messages) != 1 {
		t.Fatal("failed delivery remained in the inbox")
	}
}

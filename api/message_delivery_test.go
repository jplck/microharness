package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func deliveryTestEnvironment(t *testing.T) (context.Context, string, *AgentEnvironment, *Agent) {
	t.Helper()
	ctx, root, _ := setupPersistenceTest(t)
	env, err := NewAgentEnvironment(ctx, root, "messages")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(env.Close)
	worker, err := env.CreateAgent(ctx, "test-model", nil, "worker", "", true)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, root, env, worker
}

func assertDelivery(t *testing.T, env *AgentEnvironment, id, status string) {
	t.Helper()
	record, err := env.Delivery(id)
	if err != nil || record.Status != status {
		t.Fatalf("delivery %s: %+v %v; want %s", id, record, err, status)
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for worker")
	}
}

func TestRetractMessagePersistence(t *testing.T) {
	ctx, root, env, worker := deliveryTestEnvironment(t)
	first := Envelope{ID: "first", Source: "tui", Sender: "user", To: "worker", Content: "Original"}
	if _, err := env.Message(ctx, first); err != nil {
		t.Fatal(err)
	}
	duplicate := Envelope{ID: "duplicate", Source: "tui", Sender: "user", To: "worker", Content: "Duplicate", ConversationID: "first"}
	if _, err := env.Message(ctx, duplicate); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Message(ctx, Envelope{ID: "steer", Source: "tui", Sender: "user", To: "worker", Content: "Correction", Steer: "duplicate"}); err != nil {
		t.Fatal(err)
	}
	for _, identity := range [][2]string{{"tui", "other"}, {"agent", "user"}, {"runtime", "user"}} {
		if _, err := env.RetractMessage(ctx, identity[0], identity[1], "duplicate"); !errors.Is(err, ErrMessageOwnership) {
			t.Fatalf("wrong identity could retract: %v", err)
		}
	}
	record, err := env.RetractMessage(ctx, "tui", "user", "duplicate")
	if err != nil || record.Status != MessageRetracted || len(worker.Inbox) != 1 || worker.Inbox[0].ID != "first" {
		t.Fatalf("duplicate not retracted: %+v %v", record, err)
	}
	assertDelivery(t, env, "steer", MessageCancelled)
	if _, err := env.RetractMessage(ctx, "tui", "user", "duplicate"); err != nil {
		t.Fatal("repeated retraction failed:", err)
	}
	if _, err := env.Message(ctx, duplicate); err != nil || len(worker.Inbox) != 1 {
		t.Fatal("explicit ID retry requeued a retracted message:", err)
	}
	loaded, err := LoadAgentEnvironment(ctx, root, "messages", DefaultTools())
	if err != nil {
		t.Fatal(err)
	}
	assertDelivery(t, loaded, "duplicate", MessageRetracted)
	assertDelivery(t, loaded, "steer", MessageCancelled)
	if len(loaded.Deliveries("worker")) != 3 || len(loaded.InitialAgent.Inbox) != 1 {
		t.Fatal("restart lost retraction history or changed queued work")
	}
}

func TestRetractionGuardsAndRollback(t *testing.T) {
	ctx, root, env, worker := deliveryTestEnvironment(t)
	if _, err := env.CreateAgent(ctx, "test-model", nil, "sender", "", false); err != nil {
		t.Fatal(err)
	}
	receipt, err := env.SendAgentMessage(ctx, "sender", Envelope{To: "worker", Content: "Work"})
	if err != nil || receipt.DeliveryStatus != MessageQueued {
		t.Fatal("message receipt missing delivery state:", err)
	}
	if _, err := env.SentDelivery("worker", receipt.ID); !errors.Is(err, ErrMessageOwnership) {
		t.Fatal("another agent could inspect outgoing message:", err)
	}
	originalRoot := env.DataRoot
	env.DataRoot = filepath.Join(root, "blocked")
	if err := os.WriteFile(env.DataRoot, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := env.RetractMessage(ctx, "agent", "sender", receipt.ID); err == nil {
		t.Fatal("failed save was not reported")
	}
	env.DataRoot = originalRoot
	assertDelivery(t, env, receipt.ID, MessageQueued)
	if len(worker.Inbox) != 1 {
		t.Fatal("failed retraction lost queued work")
	}
	if err := env.DeleteAgent(ctx, "sender", false); err != nil {
		t.Fatal(err)
	}
	if _, err := env.CreateAgent(ctx, "test-model", nil, "sender", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := env.RetractMessage(ctx, "agent", "sender", receipt.ID); !errors.Is(err, ErrMessageOwnership) {
		t.Fatal("recreated agent took over old sender's messages:", err)
	}
	if err := env.DeleteAgent(ctx, "worker", false); err != nil {
		t.Fatal(err)
	}
	assertDelivery(t, env, receipt.ID, MessageCancelled)
}

func assertToolProtocol(t *testing.T, messages []Message) {
	t.Helper()
	pending := map[string]bool{}
	for _, message := range messages {
		if message.Role == "tool" {
			if !pending[message.ToolCallID] {
				t.Errorf("unexpected or repeated tool result %s", message.ToolCallID)
			}
			delete(pending, message.ToolCallID)
			continue
		}
		if len(pending) > 0 {
			t.Fatalf("message %s interrupts unresolved tool calls: %v", message.Role, pending)
		}
		for _, call := range message.ToolCalls {
			pending[call.ID] = true
		}
	}
	if len(pending) > 0 {
		t.Fatal("missing tool results:", pending)
	}
}

func TestSteeringDuringModelCall(t *testing.T) {
	for _, proposesTools := range []bool{false, true} {
		name := "final-response"
		if proposesTools {
			name = "tool-batch"
		}
		t.Run(name, func(t *testing.T) {
			ctx, _, env, worker := deliveryTestEnvironment(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var calls, effects atomic.Int32
			worker.Tools = append(worker.Tools, Tool{Name: "effect", Execute: func(context.Context, json.RawMessage) (string, error) {
				effects.Add(1)
				return "executed", nil
			}})
			worker.Client = messageTestModel{call: func(ctx context.Context, messages []Message) (Message, error) {
				switch calls.Add(1) {
				case 1:
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return Message{}, ctx.Err()
					}
					if proposesTools {
						return Message{Role: "assistant", ToolCalls: []ToolCall{
							{ID: "a", Name: "effect", Arguments: json.RawMessage(`{}`)},
							{ID: "b", Name: "effect", Arguments: json.RawMessage(`{}`)},
						}}, nil
					}
					return Message{Role: "assistant", Content: "Old answer"}, nil
				case 2:
					assertToolProtocol(t, messages)
					last := messages[len(messages)-1]
					if last.Steer != "original" || !strings.Contains(last.Content, "Use the correction") {
						t.Error("steering was not delivered before continuing")
					}
					assertDelivery(t, env, "steering", MessageDelivered)
					return Message{Role: "assistant", Content: "Corrected answer"}, nil
				default:
					t.Error("steering was processed as a separate task")
					return Message{Role: "assistant", Content: "unexpected"}, nil
				}
			}}
			if err := env.Start(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := env.Message(ctx, Envelope{ID: "original", Source: "tui", Sender: "user", To: "worker", Content: "Start"}); err != nil {
				t.Fatal(err)
			}
			waitSignal(t, entered)
			if _, err := env.RetractMessage(ctx, "tui", "user", "original"); !errors.Is(err, ErrMessageDelivered) {
				t.Fatal("delivered message could be retracted:", err)
			}
			if _, err := env.Message(ctx, Envelope{ID: "steering", Source: "tui", Sender: "user", To: "worker", Content: "Use the correction", Steer: "original"}); err != nil {
				t.Fatal(err)
			}
			assertDelivery(t, env, "steering", MessageQueued)
			close(release)
			waitForInbox(t, env, "worker", func(inbox []Envelope, failure string) bool { return len(inbox) == 0 && failure == "" })
			assertDelivery(t, env, "original", MessageCompleted)
			assertDelivery(t, env, "steering", MessageCompleted)
			if calls.Load() != 2 || effects.Load() != 0 {
				t.Fatalf("unexpected calls/effects: %d/%d", calls.Load(), effects.Load())
			}
			if _, err := env.Message(ctx, Envelope{Source: "tui", Sender: "user", To: "worker", Content: "Too late", Steer: "original"}); !errors.Is(err, ErrSteeringTarget) {
				t.Fatal("finished run accepted steering:", err)
			}
		})
	}
}

func TestSteeringBetweenToolsAndRestart(t *testing.T) {
	ctx, root, env, worker := deliveryTestEnvironment(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var effects atomic.Int32
	worker.Tools = append(worker.Tools, Tool{Name: "effect", Execute: func(ctx context.Context, arguments json.RawMessage) (string, error) {
		effects.Add(1)
		close(entered)
		select {
		case <-release:
			return "first effect happened", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}})
	var calls atomic.Int32
	worker.Client = messageTestModel{call: func(_ context.Context, messages []Message) (Message, error) {
		if calls.Add(1) == 1 {
			return Message{Role: "assistant", ToolCalls: []ToolCall{
				{ID: "first", Name: "effect", Arguments: json.RawMessage(`{}`)},
				{ID: "second", Name: "effect", Arguments: json.RawMessage(`{}`)},
			}}, nil
		}
		assertToolProtocol(t, messages)
		return Message{}, errors.New("model unavailable after steering")
	}}
	if err := env.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Message(ctx, Envelope{ID: "original", Source: "tui", Sender: "user", To: "worker", Content: "Two effects"}); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, entered)
	if _, err := env.Message(ctx, Envelope{ID: "steering", Source: "tui", Sender: "user", To: "worker", Content: "Stop remaining effects", Steer: "original"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitForInbox(t, env, "worker", func(_ []Envelope, failure string) bool { return failure != "" })
	env.Close()
	if effects.Load() != 1 {
		t.Fatal("steering failed to stop unstarted tool:", effects.Load())
	}
	// Simulate a crash after the session commit but before its delivery-state commit.
	env.setDeliveryLocked("original", MessageQueued)
	env.setDeliveryLocked("steering", MessageQueued)
	if err := env.saveLocked(); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadAgentEnvironment(ctx, root, "messages", DefaultTools())
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()
	assertDelivery(t, loaded, "steering", MessageDelivered)
	if _, err := loaded.RetractMessage(ctx, "tui", "user", "steering"); !errors.Is(err, ErrMessageDelivered) {
		t.Fatal("delivered steering could be retracted after restart:", err)
	}
	loaded.InitialAgent.Client = messageTestModel{call: func(_ context.Context, messages []Message) (Message, error) {
		assertToolProtocol(t, messages)
		steering, results := 0, 0
		for _, message := range messages {
			if message.Steer == "original" {
				steering++
			}
			if message.Role == "tool" {
				results++
			}
		}
		if steering != 1 || results != 2 {
			t.Errorf("restart duplicated or lost messages: %d steering, %d results", steering, results)
		}
		return Message{Role: "assistant", Content: "Stopped; first effect already happened"}, nil
	}}
	if err := loaded.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := loaded.RetryInbox(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	waitForInbox(t, loaded, "worker", func(inbox []Envelope, failure string) bool { return len(inbox) == 0 && failure == "" })
	assertDelivery(t, loaded, "steering", MessageCompleted)
}

func TestRetractQueuedSteeringAndIndependentMessages(t *testing.T) {
	ctx, _, env, worker := deliveryTestEnvironment(t)
	for _, envelope := range []Envelope{
		{ID: "original", Source: "tui", Sender: "user", To: "worker", Content: "Original", ConversationID: "conversation"},
		{ID: "independent", Source: "tui", Sender: "user", To: "worker", Content: "Follow-up", ConversationID: "conversation"},
		{ID: "steering", Source: "tui", Sender: "user", To: "worker", Content: "Withdrawn correction", Steer: "original"},
	} {
		if _, err := env.Message(ctx, envelope); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.RetractMessage(ctx, "tui", "user", "steering"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	worker.Client = messageTestModel{call: func(_ context.Context, messages []Message) (Message, error) {
		calls.Add(1)
		for _, message := range messages {
			if message.Steer != "" {
				t.Error("retracted steering reached the model")
			}
		}
		return Message{Role: "assistant", Content: "Done"}, nil
	}}
	if err := env.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitForInbox(t, env, "worker", func(inbox []Envelope, failure string) bool { return len(inbox) == 0 && failure == "" })
	if calls.Load() != 2 {
		t.Fatal("conversation-level deduplication lost a legitimate follow-up")
	}
	assertDelivery(t, env, "steering", MessageRetracted)
}

func TestRetractionRaceWithDelivery(t *testing.T) {
	ctx, _, env, worker := deliveryTestEnvironment(t)
	var calls atomic.Int32
	worker.Client = messageTestModel{call: func(context.Context, []Message) (Message, error) {
		calls.Add(1)
		return Message{Role: "assistant", Content: "Done"}, nil
	}}
	if err := env.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for attempt := range 20 {
		id := fmt.Sprintf("race-%d", attempt)
		before := calls.Load()
		if _, err := env.Message(ctx, Envelope{ID: id, Source: "tui", Sender: "user", To: "worker", Content: "Work"}); err != nil {
			t.Fatal(err)
		}
		_, err := env.RetractMessage(ctx, "tui", "user", id)
		if err != nil && !errors.Is(err, ErrMessageDelivered) {
			t.Fatal(err)
		}
		waitForInbox(t, env, "worker", func(inbox []Envelope, failure string) bool { return len(inbox) == 0 && failure == "" })
		if err == nil {
			assertDelivery(t, env, id, MessageRetracted)
			if calls.Load() != before {
				t.Fatal("successfully retracted message executed")
			}
		} else {
			assertDelivery(t, env, id, MessageCompleted)
			if calls.Load() != before+1 {
				t.Fatal("claimed message did not execute exactly once")
			}
		}
	}
}

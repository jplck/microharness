package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type messageDeliveryRESTClient struct {
	t      *testing.T
	client *http.Client
}

func (client messageDeliveryRESTClient) request(method, path, body string, status int) []byte {
	client.t.Helper()
	request, err := http.NewRequestWithContext(client.t.Context(), method, "http://localhost"+path, strings.NewReader(body))
	if err != nil {
		client.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.client.Do(request)
	if err != nil {
		client.t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != status {
		client.t.Fatalf("%s %s: got %s, want %d; %s (%v)", method, path, response.Status, status, data, err)
	}
	if status < 300 && len(data) > 0 && response.Header.Get("Content-Type") != "application/json" {
		client.t.Fatalf("%s %s did not return JSON", method, path)
	}
	return data
}

func decodeMessageDeliveryREST[T any](t *testing.T, data []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return value
}

func (client messageDeliveryRESTClient) delivery(id, status string) MessageDelivery {
	client.t.Helper()
	record := decodeMessageDeliveryREST[MessageDelivery](client.t,
		client.request(http.MethodGet, "/environments/mail/messages/"+url.PathEscape(id), "", http.StatusOK))
	if record.Envelope.ID != id || record.Status != status || record.CreatedAt.IsZero() {
		client.t.Fatalf("delivery %q: %+v; want %s with creation timestamp", id, record, status)
	}
	return record
}

func TestMessageDeliveryRESTQueuedControls(t *testing.T) {
	ctx, root, _ := setupPersistenceTest(t)
	env, err := NewAgentEnvironment(ctx, root, "mail")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(env.Close)
	worker, err := env.CreateAgent(ctx, "test-model", nil, "worker", "", true)
	if err != nil {
		t.Fatal(err)
	}
	other, err := env.CreateAgent(ctx, "test-model", nil, "other", "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, envelope := range []Envelope{
		{ID: "original", Source: "tui", Sender: "alice", To: "worker", Content: "Original request"},
		{ID: "duplicate", Source: "tui", Sender: "alice", To: "worker", Content: "Duplicate request", ConversationID: "original"},
		{ID: "steering", Source: "tui", Sender: "alice", To: "worker", Content: "Correction", Steer: "duplicate"},
		{ID: "outgoing", Source: "agent", Sender: "worker", To: "other", Content: "Delegated work"},
		{ID: "unrelated", Source: "cli", Sender: "alice", To: "other", Content: "Other work"},
	} {
		if _, err := env.Message(ctx, envelope); err != nil {
			t.Fatal(err)
		}
	}
	worker.InboxError, other.InboxError = "paused fixture", "paused fixture"
	// Keep the paused fixture from generating unrelated failure notifications.
	other.notifiedFailures = []string{"outgoing"}
	if err := env.saveLocked(); err != nil {
		t.Fatal(err)
	}
	env.Close()
	httpClient, stop := startTestRuntime(t, root)
	client := messageDeliveryRESTClient{t: t, client: httpClient}
	for _, path := range []string{
		"/environments/missing/messages",
		"/environments/missing/messages/duplicate",
		"/environments/mail/messages/missing",
	} {
		client.request(http.MethodGet, path, "", http.StatusNotFound)
	}
	client.request(http.MethodPost, "/environments/missing/messages/duplicate/retract", `{"Source":"tui","Sender":"alice"}`, http.StatusNotFound)
	client.request(http.MethodPost, "/environments/mail/messages/missing/retract", `{"Source":"tui","Sender":"alice"}`, http.StatusNotFound)

	receipt := decodeMessageDeliveryREST[map[string]string](t,
		client.request(http.MethodPost, "/environments/mail/messages",
			`{"ID":"external","Source":"tui","Sender":"alice","To":"worker","Content":"Queued through HTTP"}`, http.StatusAccepted))
	if receipt["id"] != "external" || receipt["status"] != "accepted" || receipt["delivery_status"] != MessageQueued {
		t.Fatalf("invalid acceptance receipt: %v", receipt)
	}
	client.delivery("external", MessageQueued)
	original := client.delivery("duplicate", MessageQueued)
	steering := client.delivery("steering", MessageQueued)
	if steering.Envelope.Steer != "duplicate" || steering.Envelope.ConversationID != "original" {
		t.Fatalf("steering metadata lost: %+v", steering)
	}
	for _, test := range []struct {
		body   string
		status int
	}{
		{`{"Source":"tui","Sender":"bob"}`, http.StatusForbidden},
		{`{"Source":"cli","Sender":"alice"}`, http.StatusForbidden},
		{`{"Source":"agent","Sender":"alice"}`, http.StatusBadRequest},
		{`{"Source":"runtime","Sender":"alice"}`, http.StatusBadRequest},
		{`{"Source":"tui"}`, http.StatusBadRequest},
		{`{"Sender":"alice"}`, http.StatusBadRequest},
		{`{"Source":"tui","Sender":"alice","extra":true}`, http.StatusBadRequest},
		{`{"Source":"tui","Sender":"alice"} {}`, http.StatusBadRequest},
		{`{`, http.StatusBadRequest},
	} {
		client.request(http.MethodPost, "/environments/mail/messages/duplicate/retract", test.body, test.status)
		client.delivery("duplicate", MessageQueued)
	}
	for _, source := range []string{"agent", "runtime"} {
		client.request(http.MethodPost, "/environments/mail/messages",
			`{"Source":"`+source+`","Sender":"worker","To":"worker","Content":"Spoofed"}`, http.StatusBadRequest)
	}
	for _, body := range []string{
		`{"Source":"tui","Sender":"alice","To":"worker","Content":"Missing target","Steer":"missing"}`,
		`{"Source":"tui","Sender":"alice","To":"other","Content":"Wrong recipient","Steer":"original"}`,
		`{"Source":"tui","Sender":"alice","To":"worker","Content":"Steering another steer","Steer":"steering"}`,
	} {
		client.request(http.MethodPost, "/environments/mail/messages", body, http.StatusConflict)
	}

	retracted := decodeMessageDeliveryREST[MessageDelivery](t, client.request(http.MethodPost,
		"/environments/mail/messages/duplicate/retract", `{"Source":"tui","Sender":"alice"}`, http.StatusOK))
	if retracted.Status != MessageRetracted || retracted.Envelope != original.Envelope || !retracted.CreatedAt.Equal(original.CreatedAt) {
		t.Fatalf("retraction changed original envelope: %+v", retracted)
	}
	repeated := decodeMessageDeliveryREST[MessageDelivery](t, client.request(http.MethodPost,
		"/environments/mail/messages/duplicate/retract", `{"Source":"tui","Sender":"alice"}`, http.StatusOK))
	if !reflect.DeepEqual(repeated, retracted) {
		t.Fatalf("retraction was not idempotent: %+v / %+v", retracted, repeated)
	}
	client.request(http.MethodPost, "/environments/mail/messages/duplicate/retract", `{"Source":"tui","Sender":"bob"}`, http.StatusForbidden)
	client.delivery("duplicate", MessageRetracted)
	client.delivery("steering", MessageCancelled)
	client.delivery("original", MessageQueued)
	client.request(http.MethodPost, "/environments/mail/messages",
		`{"Source":"tui","Sender":"alice","To":"worker","Content":"Withdrawn target","Steer":"duplicate"}`, http.StatusConflict)

	checkHistory := func() {
		t.Helper()
		for _, test := range []struct {
			query string
			ids   []string
		}{
			{"", []string{"original", "duplicate", "steering", "outgoing", "unrelated", "external"}},
			{"?agent=worker", []string{"original", "duplicate", "steering", "outgoing", "external"}},
			{"?agent=other", []string{"outgoing", "unrelated"}},
			{"?agent=missing", []string{}},
		} {
			history := decodeMessageDeliveryREST[[]MessageDelivery](t,
				client.request(http.MethodGet, "/environments/mail/messages"+test.query, "", http.StatusOK))
			ids := make([]string, 0, len(history))
			for _, record := range history {
				ids = append(ids, record.Envelope.ID)
			}
			if !reflect.DeepEqual(ids, test.ids) {
				t.Fatalf("history %q: got %v, want %v", test.query, ids, test.ids)
			}
		}
		inbox := decodeMessageDeliveryREST[InboxState](t,
			client.request(http.MethodGet, "/environments/mail/agents/worker/inbox", "", http.StatusOK))
		if len(inbox.Messages) != 2 || inbox.Messages[0].ID != "original" || inbox.Messages[1].ID != "external" || inbox.Status != "paused" {
			t.Fatalf("retraction disturbed other queued work: %+v", inbox)
		}
	}
	checkHistory()
	stop()
	httpClient, _ = startTestRuntime(t, root)
	client.client = httpClient
	checkHistory()
	if persisted := client.delivery("duplicate", MessageRetracted); !reflect.DeepEqual(persisted, retracted) {
		t.Fatalf("restart changed delivery: %+v / %+v", retracted, persisted)
	}
	client.delivery("steering", MessageCancelled)
}

func TestMessageDeliveryRESTLiveSteering(t *testing.T) {
	ctx, root, _ := setupPersistenceTest(t)
	started, continued := make(chan struct{}), make(chan struct{})
	releaseInitial, releaseFinal := make(chan struct{}), make(chan struct{})
	var releaseInitialOnce, releaseFinalOnce sync.Once
	unblockInitial := func() { releaseInitialOnce.Do(func() { close(releaseInitial) }) }
	unblockFinal := func() { releaseFinalOnce.Do(func() { close(releaseFinal) }) }
	var calls atomic.Int32
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var request struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode model request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var release <-chan struct{}
		answer := "Original answer"
		switch calls.Add(1) {
		case 1:
			close(started)
			release = releaseInitial
		case 2:
			last := request.Messages[len(request.Messages)-1]
			if last.Role != "user" || !strings.Contains(last.Content, `"Steer":"original"`) || !strings.Contains(last.Content, "Use the corrected direction") {
				t.Errorf("continuation did not receive steering: %+v", last)
			}
			close(continued)
			release, answer = releaseFinal, "Corrected answer"
		default:
			t.Error("steering became a separate run")
			answer = "Unexpected extra answer"
		}
		if release != nil {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"id": "reply", "object": "chat.completion",
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]string{"role": "assistant", "content": answer},
			}},
		}); err != nil {
			t.Errorf("encode model response: %v", err)
		}
	}))
	t.Cleanup(modelServer.Close)
	t.Cleanup(func() { unblockInitial(); unblockFinal() })
	if err := writeJSONAtomic("models.json", map[string]any{"models": []Model{{Name: "test-model", Provider: ProviderOpenAI, Endpoint: modelServer.URL}}}); err != nil {
		t.Fatal(err)
	}
	httpClient, stop := startTestRuntime(t, root)
	client := messageDeliveryRESTClient{t: t, client: httpClient}
	client.request(http.MethodPost, "/environments/mail", "", http.StatusCreated)
	client.request(http.MethodPost, "/environments/mail/agents/worker?model=test-model", "", http.StatusCreated)
	receipt := decodeMessageDeliveryREST[map[string]string](t,
		client.request(http.MethodPost, "/environments/mail/messages",
			`{"ID":"original","Source":"tui","Sender":"alice","To":"worker","Content":"Original request","ConversationID":"thread"}`, http.StatusAccepted))
	if receipt["id"] != "original" || receipt["status"] != "accepted" || (receipt["delivery_status"] != MessageQueued && receipt["delivery_status"] != MessageDelivered) {
		t.Fatalf("invalid live receipt: %v", receipt)
	}
	waitSignal(t, started)
	client.delivery("original", MessageDelivered)
	client.request(http.MethodPost, "/environments/mail/messages/original/retract", `{"Source":"tui","Sender":"alice"}`, http.StatusConflict)
	client.request(http.MethodPost, "/environments/mail/messages/original/retract", `{"Source":"tui","Sender":"bob"}`, http.StatusForbidden)
	receipt = decodeMessageDeliveryREST[map[string]string](t,
		client.request(http.MethodPost, "/environments/mail/messages",
			`{"ID":"steering","Source":"tui","Sender":"alice","To":"worker","Content":"Use the corrected direction","Steer":"original"}`, http.StatusAccepted))
	if receipt["id"] != "steering" || receipt["status"] != "accepted" || receipt["delivery_status"] != MessageQueued {
		t.Fatalf("invalid steering receipt: %v", receipt)
	}
	steering := client.delivery("steering", MessageQueued)
	if steering.Envelope.Steer != "original" || steering.Envelope.ConversationID != "thread" {
		t.Fatalf("steering did not inherit original routing: %+v", steering)
	}
	unblockInitial()
	waitSignal(t, continued)
	client.delivery("original", MessageDelivered)
	client.delivery("steering", MessageDelivered)
	client.request(http.MethodPost, "/environments/mail/messages/steering/retract", `{"Source":"tui","Sender":"alice"}`, http.StatusConflict)
	saved, err := LoadAgentEnvironment(ctx, root, "mail", DefaultTools())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(saved.Close)
	for _, id := range []string{"original", "steering"} {
		record, err := saved.Delivery(id)
		if err != nil || record.Status != MessageDelivered {
			t.Fatalf("delivered checkpoint not persisted for %s: %+v %v", id, record, err)
		}
	}
	unblockFinal()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		record := decodeMessageDeliveryREST[MessageDelivery](t,
			client.request(http.MethodGet, "/environments/mail/messages/original", "", http.StatusOK))
		if record.Status == MessageCompleted {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("original delivery never completed: %+v", record)
		}
	}
	completed := map[string]MessageDelivery{}
	for _, id := range []string{"original", "steering"} {
		completed[id] = client.delivery(id, MessageCompleted)
		client.request(http.MethodPost, "/environments/mail/messages/"+id+"/retract", `{"Source":"tui","Sender":"alice"}`, http.StatusConflict)
	}
	client.request(http.MethodPost, "/environments/mail/messages",
		`{"Source":"tui","Sender":"alice","To":"worker","Content":"Too late","Steer":"original"}`, http.StatusConflict)
	session := decodeMessageDeliveryREST[Session](t,
		client.request(http.MethodGet, "/environments/mail/agents/worker/session", "", http.StatusOK))
	last := session.Messages[len(session.Messages)-1]
	if last.Content != "Corrected answer" || last.RunID != "original" || calls.Load() != 2 {
		t.Fatalf("steering did not continue original run: final=%+v, calls=%d", last, calls.Load())
	}
	stop()
	httpClient, _ = startTestRuntime(t, root)
	client.client = httpClient
	for id, previous := range completed {
		if record := client.delivery(id, MessageCompleted); !reflect.DeepEqual(record, previous) {
			t.Fatalf("restart changed completed delivery %s: %+v / %+v", id, previous, record)
		}
	}
	history := decodeMessageDeliveryREST[[]MessageDelivery](t,
		client.request(http.MethodGet, "/environments/mail/messages?agent=worker", "", http.StatusOK))
	if len(history) != 2 || history[0].Envelope.ID != "original" || history[1].Envelope.ID != "steering" || history[1].Status != MessageCompleted {
		t.Fatalf("restart lost completed steering history: %+v", history)
	}
	if calls.Load() != 2 {
		t.Fatal("restart replayed completed messages")
	}
}

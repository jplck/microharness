package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func startTestRuntime(t *testing.T, root string) (*http.Client, func()) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "runtime.sock")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var serveErr error
	go func() {
		defer close(done)
		serveErr = ServeRuntime(ctx, socket, root)
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
			if serveErr != nil {
				t.Errorf("runtime failed: %v", serveErr)
			}
		case <-time.After(10 * time.Second):
			t.Error("runtime did not shut down")
		}
	}
	t.Cleanup(stop)
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: time.Second}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		response, err := client.Get("http://localhost/environments")
		if err == nil {
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("readiness returned %s", response.Status)
			}
			return client, stop
		}
		select {
		case <-done:
			t.Fatalf("runtime stopped before readiness: %v", serveErr)
		case <-deadline.C:
			t.Fatalf("runtime not ready: %v", err)
		case <-ticker.C:
		}
	}
}

func TestRuntimeRestoresEnvironments(t *testing.T) {
	ctx, root, _ := setupPersistenceTest(t)
	registry := BuiltinTools()
	cli, err := NewAgentEnvironment(ctx, root, "cli-environment")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.CreateAgent(ctx, "test-model", []Tool{registry["get_time"]}, "cli-agent", "", true); err != nil {
		t.Fatal(err)
	}
	client, stop := startTestRuntime(t, root)
	post := func(path string, wantStatus int) {
		t.Helper()
		response, err := client.Post("http://localhost"+path, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != wantStatus {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("POST %s: got %s (%s), want %d", path, response.Status, body, wantStatus)
		}
	}
	post("/environments/test", http.StatusCreated)
	instructions := "Research technical questions & report findings.\nInclude sources + note uncertainty?"
	query := url.Values{"model": []string{"test-model"}, "instructions": []string{instructions}}
	post("/environments/test/agents/assistant?"+query.Encode(), http.StatusCreated)
	stop()
	loaded, err := LoadAgentEnvironment(ctx, root, "test", registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Agents) != 1 || loaded.InitialAgent != loaded.Agents[0] {
		t.Fatal("REST-created agent was not persisted")
	}
	if loaded.InitialAgent.Instructions != instructions || len(loaded.InitialAgent.Session.Messages) == 0 ||
		loaded.InitialAgent.Session.Messages[0].Role != "system" ||
		!strings.HasSuffix(loaded.InitialAgent.Session.Messages[0].Content, "Agent-specific instructions:\n"+instructions) {
		t.Fatal("custom instructions were not persisted or added to the system prompt")
	}
	client, stop = startTestRuntime(t, root)
	response, err := client.Get("http://localhost/environments")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var names []string
	if err := json.NewDecoder(response.Body).Decode(&names); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"cli-environment", "test"}) {
		t.Fatalf("environments not restored: %v", names)
	}
	post("/environments/test", http.StatusConflict)
	post("/environments/test/agents/assistant?model=test-model", http.StatusConflict)
	post("/environments/test/agents/second?model=test-model", http.StatusCreated)
	stop()
	restored, err := LoadAgentEnvironment(ctx, root, "test", registry)
	if err != nil {
		t.Fatal(err)
	}
	if restored.ID != loaded.ID || len(restored.Agents) != 2 || restored.InitialAgent.Session.SessionID != loaded.InitialAgent.Session.SessionID {
		t.Fatal("restart lost original identity, agents, or session")
	}
	if restored.InitialAgent.Instructions != instructions ||
		restored.InitialAgent.Session.Messages[0].Content != loaded.InitialAgent.Session.Messages[0].Content {
		t.Fatal("restart changed custom instructions or the system prompt")
	}
	if restored.Agents[1].Instructions != "" || strings.Contains(restored.Agents[1].Session.Messages[0].Content, "Agent-specific instructions:") {
		t.Fatal("omitting instructions changed the default prompt")
	}
}

func TestRuntimeMessagingAPI(t *testing.T) {
	ctx, root, registry := setupPersistenceTest(t)
	var available atomic.Bool
	var calls atomic.Int32
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if !available.Load() {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"model unavailable","type":"invalid_request_error"}}`)
			return
		}
		io.WriteString(w, `{"id":"reply","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Completed"}}]}`)
	}))
	defer modelServer.Close()
	if err := writeJSONAtomic("models.json", map[string]any{"models": []Model{{Name: "test-model", Provider: ProviderOpenAI, Endpoint: modelServer.URL}}}); err != nil {
		t.Fatal(err)
	}
	client, stop := startTestRuntime(t, root)
	request := func(method, path, body string, status int) []byte {
		t.Helper()
		req, err := http.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != status {
			t.Fatalf("%s %s returned %s: %s", method, path, response.Status, data)
		}
		return data
	}
	request(http.MethodPost, "/environments/mail", "", http.StatusCreated)
	request(http.MethodPost, "/environments/mail/agents/recipient?model=test-model", "", http.StatusCreated)
	for _, body := range []string{
		`{"Source":"agent","Sender":"recipient","Content":"spoofed"}`,
		`{"Sender":"user","Content":""}`,
		`{"Content":"missing sender"}`,
		`{"Sender":"user","Content":"extra JSON"} {}`,
		`{"Sender":"user","Content":"unknown field","unknown":1}`,
	} {
		request(http.MethodPost, "/environments/mail/messages", body, http.StatusBadRequest)
	}
	request(http.MethodPost, "/environments/missing/messages", `{}`, http.StatusNotFound)
	request(http.MethodPost, "/environments/mail/messages", `{"Sender":"user","To":"missing","Content":"hello"}`, http.StatusNotFound)
	request(http.MethodGet, "/environments/mail/agents/missing/inbox", "", http.StatusNotFound)
	request(http.MethodPost, "/environments/mail/agents/missing/inbox/retry", "", http.StatusNotFound)
	data := request(http.MethodPost, "/environments/mail/messages", `{"Sender":"user","Content":"First job","ConversationID":"thread-1"}`, http.StatusAccepted)
	var receipt map[string]string
	if err := json.Unmarshal(data, &receipt); err != nil || receipt["id"] == "" || receipt["status"] != "accepted" {
		t.Fatalf("invalid receipt: %s, %v", data, err)
	}
	waitInbox := func(ready func(InboxState) bool) {
		t.Helper()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for {
			data := request(http.MethodGet, "/environments/mail/agents/recipient/inbox", "", http.StatusOK)
			var inbox InboxState
			if err := json.Unmarshal(data, &inbox); err != nil {
				t.Fatal(err)
			}
			if ready(inbox) {
				return
			}
			select {
			case <-ticker.C:
			case <-deadline.C:
				t.Fatalf("inbox state did not match: %s", data)
			}
		}
	}
	waitInbox(func(inbox InboxState) bool { return len(inbox.Messages) == 1 && inbox.Error != "" })
	stop()
	available.Store(true)
	client, stop = startTestRuntime(t, root)
	request(http.MethodPost, "/environments/mail/messages", `{"Sender":"user","To":"recipient","Content":"Second job"}`, http.StatusAccepted)
	waitInbox(func(inbox InboxState) bool { return len(inbox.Messages) == 2 && inbox.Error != "" })
	request(http.MethodPost, "/environments/mail/agents/recipient/inbox/retry", "", http.StatusAccepted)
	waitInbox(func(inbox InboxState) bool { return len(inbox.Messages) == 0 && inbox.Error == "" })
	stop()
	loaded, err := LoadAgentEnvironment(ctx, root, "mail", registry)
	if err != nil {
		t.Fatal(err)
	}
	inputs := 0
	for _, message := range loaded.InitialAgent.Session.Messages {
		if message.EnvelopeID != "" {
			inputs++
		}
	}
	if calls.Load() != 3 || inputs != 2 {
		t.Fatalf("unexpected model calls or duplicated inputs: %d calls, %d inputs", calls.Load(), inputs)
	}
}

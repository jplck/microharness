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
	agentResponse, err := client.Get("http://localhost/environments/test/agents")
	if err != nil {
		t.Fatal(err)
	}
	var agents []AgentSummary
	err = json.NewDecoder(agentResponse.Body).Decode(&agents)
	agentResponse.Body.Close()
	if err != nil || agentResponse.StatusCode != http.StatusOK || len(agents) != 1 ||
		agents[0].Name != "assistant" || agents[0].Model != "test-model" || agents[0].Instructions != instructions {
		t.Fatalf("agent listing: %+v, %v", agents, err)
	}
	for _, path := range []string{"/environments/missing/agents/assistant/session", "/environments/test/agents/missing/session"} {
		response, err := client.Get("http://localhost" + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s: got %s", path, response.Status)
		}
	}
	for _, content := range []string{"First completed response", "Second completed response"} {
		if err := cli.InitialAgent.Session.AddMessage(Message{Role: "assistant", Content: content}); err != nil {
			t.Fatal(err)
		}
		response, err := client.Get("http://localhost/environments/cli-environment/agents/cli-agent/session")
		if err != nil {
			t.Fatal(err)
		}
		var snapshot Session
		err = json.NewDecoder(response.Body).Decode(&snapshot)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || len(snapshot.Messages) == 0 {
			t.Fatalf("session snapshot: %s, %v", response.Status, err)
		}
		if snapshot.Messages[len(snapshot.Messages)-1].Content != content {
			t.Fatal("session endpoint did not read the latest committed message")
		}
		if snapshot.Scope != "" || snapshot.SessionID != "" {
			t.Fatal("session endpoint exposed internal storage paths")
		}
	}
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
	if response.Header.Get("Content-Type") != "application/json" {
		t.Fatal("environment list is not a JSON response")
	}
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

func TestRuntimeToolSelection(t *testing.T) {
	ctx, root, _ := setupPersistenceTest(t)
	if err := writeJSONAtomic("models.json", map[string]any{"models": []Model{{Name: "test-model", Provider: ProviderOpenAI}, {Name: "other-model", Provider: ProviderOpenAI}}}); err != nil {
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
		if err != nil || response.StatusCode != status {
			t.Fatalf("%s %s: %s %s %v", method, path, response.Status, data, err)
		}
		return data
	}
	var catalog []ToolSummary
	if err := json.Unmarshal(request("GET", "/tools", "", 200), &catalog); err != nil || len(catalog) != 2 || catalog[0].Name != "create_agent" || catalog[1].Name != "get_time" {
		t.Fatalf("tool catalog: %+v %v", catalog, err)
	}
	request("POST", "/environments/tools", "", 201)
	for _, query := range []string{"tool=missing", "tool=get_time&tool=get_time", "tool=message", "assignable_tool=missing", "assignable_tool=get_time&assignable_tool=get_time", "assignable_tool=message"} {
		request("POST", "/environments/tools/agents/assistant?model=test-model&"+query, "", 400)
	}
	request("POST", "/environments/tools/agents/assistant?model=test-model&tool=get_time&assignable_tool=create_agent", "", 201)
	var agents []AgentSummary
	if err := json.Unmarshal(request("GET", "/environments/tools/agents", "", 200), &agents); err != nil || len(agents) != 1 || !reflect.DeepEqual(agents[0].Tools, []string{"get_time"}) {
		t.Fatalf("agent tools: %+v %v", agents, err)
	}
	if !reflect.DeepEqual(agents[0].AssignableTools, []string{"create_agent"}) {
		t.Fatal("assign-only permission not returned")
	}
	for _, body := range []string{`["missing"]`, `["get_time","get_time"]`, `null`, `{}`, `[] []`} {
		request("PUT", "/environments/tools/agents/assistant/tools", body, 400)
	}
	request("PUT", "/environments/tools/agents/missing/tools", `[]`, 404)
	request("PUT", "/environments/tools/agents/assistant/tools", `[]`, 204)
	request("PUT", "/environments/tools/agents/assistant/tools", `["get_time"]`, 204)
	instructions := "Updated instructions\nKeep sources & caveats."
	update := "/environments/tools/agents/assistant?" + url.Values{"model": {"other-model"}, "instructions": {instructions}, "tool": {"get_time"}, "assignable_tool": {"get_time"}}.Encode()
	request("PUT", update, "", 204)
	for _, query := range []string{"model=missing", "model=test-model&tool=missing", "model=test-model&tool=get_time&tool=get_time", "model=test-model&assignable_tool=missing", "model=test-model&assignable_tool=get_time&assignable_tool=get_time"} {
		request("PUT", "/environments/tools/agents/assistant?"+query, "", 400)
	}
	request("PUT", "/environments/tools/agents/missing?model=test-model", "", 404)
	if err := json.Unmarshal(request("GET", "/environments/tools/agents", "", 200), &agents); err != nil || agents[0].Model != "other-model" || agents[0].Instructions != instructions || !reflect.DeepEqual(agents[0].Tools, []string{"get_time"}) {
		t.Fatalf("updated agent settings: %+v %v", agents, err)
	}
	if !reflect.DeepEqual(agents[0].AssignableTools, []string{"get_time"}) {
		t.Fatal("update did not replace assign permissions")
	}
	request("PUT", "/environments/tools/agents/assistant?model=other-model&tool=get_time", "", 204)
	if err := json.Unmarshal(request("GET", "/environments/tools/agents", "", 200), &agents); err != nil || len(agents[0].AssignableTools) != 0 {
		t.Fatal("omitted assign permissions were not cleared")
	}
	request("PUT", update, "", 204)
	var session Session
	if err := json.Unmarshal(request("GET", "/environments/tools/agents/assistant/session", "", 200), &session); err != nil || len(session.Messages) == 0 || session.Messages[0].Content != agentInstructions(instructions) {
		t.Fatalf("updated session instructions: %+v %v", session, err)
	}
	request("PUT", "/environments/tools/agents/assistant/tools", `["get_time","create_agent"]`, 204)
	stop()
	registry := BuiltinTools()
	registry["create_agent"] = CreateAgentTool()
	loaded, err := LoadAgentEnvironment(ctx, root, "tools", registry)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Agents[0].ToolNames, []string{"get_time", "create_agent"}) {
		t.Fatal("tool edit did not survive restart")
	}
	if !reflect.DeepEqual(loaded.Agents[0].AssignableToolNames, []string{"get_time"}) {
		t.Fatal("use-only update or restart lost assign permissions")
	}
	if loaded.Agents[0].ModelName != "other-model" || loaded.Agents[0].Instructions != instructions || loaded.Agents[0].Session.Messages[0].Content != agentInstructions(instructions) {
		t.Fatal("agent edit did not survive restart")
	}
}

func TestRuntimeMessagingAPI(t *testing.T) {
	ctx, root, registry := setupPersistenceTest(t)
	var available atomic.Bool
	var calls atomic.Int32
	modelStarted := make(chan struct{}, 2)
	releaseModel := make(chan struct{})
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if !available.Load() {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"model unavailable","type":"invalid_request_error"}}`)
			return
		}
		modelStarted <- struct{}{}
		<-releaseModel
		io.WriteString(w, `{"id":"reply","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Completed"}}]}`)
	}))
	defer modelServer.Close()
	defer func() {
		select {
		case <-releaseModel:
		default:
			close(releaseModel)
		}
	}()
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
		if status < 300 && len(data) > 0 && response.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("%s %s is not a JSON response", method, path)
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
	request(http.MethodPost, "/environments/missing/agents/recipient?model=test-model", "", http.StatusNotFound)
	request(http.MethodGet, "/environments/missing/agents/recipient/inbox", "", http.StatusNotFound)
	request(http.MethodPost, "/environments/missing/agents/recipient/inbox/retry", "", http.StatusNotFound)
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
	select {
	case <-modelStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not start processing")
	}
	data = request(http.MethodGet, "/environments/mail/agents/recipient/session", "", http.StatusOK)
	var snapshot Session
	if err := json.Unmarshal(data, &snapshot); err != nil || len(snapshot.Messages) == 0 {
		t.Fatalf("cannot read session during an active turn: %v", err)
	}
	if snapshot.Messages[len(snapshot.Messages)-1].EnvelopeID != receipt["id"] {
		t.Fatal("active session snapshot is missing the in-progress envelope")
	}
	close(releaseModel)
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

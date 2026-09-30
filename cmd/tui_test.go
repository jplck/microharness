package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	api "github.com/jplck/micro/api"
	"github.com/muesli/termenv"
)

type tuiTransport func(*http.Request) (*http.Response, error)

func (transport tuiTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func tuiTestClient(handler http.HandlerFunc) *http.Client {
	return &http.Client{Transport: tuiTransport(func(request *http.Request) (*http.Response, error) {
		response := httptest.NewRecorder()
		handler(response, request)
		return response.Result(), nil
	})}
}

func tuiKey(value string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(value)} }

func TestTUICreateAgentAndMessage(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("models.json", []byte(`{"models":[{"name":"first","provider":"openai"},{"name":"second","provider":"openai"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var createdInstructions string
	var sent api.Envelope
	client := tuiTestClient(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/tools":
			io.WriteString(w, `[{"name":"get_time","description":"Read time"}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/environments":
			io.WriteString(w, `["team"]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/agents"):
			io.WriteString(w, `[{"name":"researcher","model":"second","pending":0}]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/agents/researcher"):
			createdInstructions = r.URL.Query().Get("instructions")
			if r.URL.Query().Get("model") != "second" {
				t.Error("selected model was not sent")
			}
			if r.URL.Query().Get("tool") != "get_time" {
				t.Error("selected tool was not sent")
			}
			if r.URL.Query().Get("assignable_tool") != "get_time" {
				t.Error("assign permission not sent")
			}
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages"):
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"id":"envelope-1","status":"accepted"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	model := newTUI(t.Context(), client)
	model.Update(model.Init()())
	_, command := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if model.page != "agents" || !model.busy {
		t.Fatal("navigation did not update model")
	}
	model.Update(command())
	_, command = model.Update(tuiKey("n"))
	model.Update(command())
	if model.form != "agent" {
		t.Fatal("agent form did not open")
	}
	model.inputs[0].SetValue("researcher")
	model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model.Update(tea.KeyMsg{Type: tea.KeyRight})
	model.Update(tea.KeyMsg{Type: tea.KeyTab})
	wantInstructions := "Research & summarize.\nKeep sources + caveats."
	model.text.SetValue(wantInstructions)
	model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model.Update(tuiKey(" "))
	model.Update(tea.KeyMsg{Type: tea.KeyRight})
	model.Update(tuiKey(" "))
	_, command = model.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if !model.busy || command == nil {
		t.Fatal("submission did not start")
	}
	_, reload := model.Update(command())
	if createdInstructions != wantInstructions || model.form != "" {
		t.Fatal("instructions lost or form not closed")
	}
	model.Update(reload())
	model.Update(tuiKey("s"))
	model.text.SetValue("Hello\nPlease investigate.")
	model.inputs[1].SetValue("conversation-1")
	model.inputs[2].SetValue("parent-1")
	_, command = model.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	model.Update(command())
	if sent.To != "researcher" || sent.Source != "tui" || sent.Sender != "tui" || sent.Content != "Hello\nPlease investigate." || sent.ConversationID != "conversation-1" || sent.ReplyTo != "parent-1" {
		t.Fatalf("message fields changed: %+v", sent)
	}
	if !strings.Contains(model.status, "envelope-1") {
		t.Fatal("missing delivery receipt")
	}
}

func TestTUIDeleteAgent(t *testing.T) {
	for _, test := range []struct {
		name    string
		erase   bool
		status  int
		message string
		removed bool
	}{
		{"keep files", false, http.StatusNoContent, "", true},
		{"erase files", true, http.StatusNoContent, "", true},
		{"running", true, http.StatusConflict, "cannot delete a running agent", false},
		{"cleanup failure", true, http.StatusInternalServerError, "agent deleted, but some saved files could not be erased", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			agents := []api.AgentSummary{{Name: "first"}, {Name: "worker", Pending: 2, Status: "paused"}}
			deletes, loads := 0, 0
			client := tuiTestClient(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodDelete && r.URL.Path == "/environments/team/agents/worker":
					deletes++
					if (r.URL.Query().Get("erase_files") == "true") != test.erase {
						t.Error("incorrect file-erasure choice")
					}
					if test.removed {
						agents = agents[:1]
					}
					if test.status != http.StatusNoContent {
						http.Error(w, test.message, test.status)
					} else {
						w.WriteHeader(http.StatusNoContent)
					}
				case r.Method == http.MethodGet && r.URL.Path == "/environments/team/agents":
					loads++
					json.NewEncoder(w).Encode(agents)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			model := newTUI(t.Context(), client)
			model.page, model.environment, model.busy = "agents", "team", false
			model.agents, model.cursor = agents, 1
			if _, command := model.Update(tuiKey("d")); command != nil || model.deletingAgent != "worker" || model.eraseAgentFiles {
				t.Fatal("delete did not open a keep-files confirmation")
			}
			model.Update(tea.KeyMsg{Type: tea.KeyDown})
			model.Update(tea.KeyMsg{Type: tea.KeyEsc})
			if model.deletingAgent != "" || deletes != 0 {
				t.Fatal("cancel deleted the agent")
			}
			model.Update(tuiKey("d"))
			if model.eraseAgentFiles {
				t.Fatal("file-erasure choice was not reset to the safe default")
			}
			for _, size := range [][2]int{{40, 22}, {80, 24}, {120, 40}} {
				model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				view := ansi.Strip(model.View())
				for _, label := range []string{"DELETE AGENT", "Agent: worker", "> [x] Keep files", "Erase files permanently", "Queued/paused work is discarded.", "enter delete", "esc cancel"} {
					if !strings.Contains(strings.Join(strings.Fields(view), " "), label) {
						t.Fatalf("missing confirmation label %q at %v:\n%s", label, size, view)
					}
				}
				if lipgloss.Height(view) > size[1] || lipgloss.Width(view) > size[0] {
					t.Fatalf("confirmation exceeds terminal bounds at %v:\n%s", size, view)
				}
			}
			model.Update(tea.KeyMsg{Type: tea.KeyDown})
			model.Update(tea.KeyMsg{Type: tea.KeyUp})
			if model.eraseAgentFiles {
				t.Fatal("up did not select keep files")
			}
			if test.erase {
				model.Update(tuiKey(" "))
				if !strings.Contains(ansi.Strip(model.View()), "> [x] Erase files permanently") {
					t.Fatal("erase selection not visible")
				}
			}
			for _, key := range []tea.KeyMsg{tuiKey("d"), tuiKey("n"), {Type: tea.KeyCtrlS}} {
				if _, command := model.Update(key); command != nil {
					t.Fatal("unconfirmed deletion submitted")
				}
			}
			_, command := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
			if command == nil || !model.busy || deletes != 0 {
				t.Fatal("confirmation did not schedule deletion")
			}
			if _, duplicate := model.Update(tea.KeyMsg{Type: tea.KeyEnter}); duplicate != nil {
				t.Fatal("repeated confirmation submitted twice")
			}
			_, reload := model.Update(command())
			if reload == nil || model.deletingAgent != "" {
				t.Fatal("deletion did not close confirmation and refresh")
			}
			model.Update(reload())
			if deletes != 1 || loads != 1 || model.busy || model.failed != (test.status != http.StatusNoContent) {
				t.Fatal("incorrect deletion result state")
			}
			if len(model.agents) != len(agents) || model.cursor >= len(agents) {
				t.Fatal("list or selection was not refreshed")
			}
			if test.message != "" && !strings.Contains(model.status, test.message) {
				t.Fatal("deletion error was hidden:", model.status)
			}
		})
	}
	model := newTUI(t.Context(), nil)
	model.page, model.busy = "agents", false
	if _, command := model.Update(tuiKey("d")); command != nil || model.deletingAgent != "" {
		t.Fatal("empty agent list allowed deletion")
	}
}

func TestTUIEditAgent(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("models.json", []byte(`{"models":[{"name":"first","provider":"openai"},{"name":"second","provider":"openai"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var submitted []string
	var submittedAssignable []string
	var submittedModel, submittedInstructions string
	puts := 0
	reject := true
	client := tuiTestClient(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /tools":
			io.WriteString(w, `[{"name":"first","description":"First tool"},{"name":"second","description":"Second tool"},{"name":"third","description":"Third tool"}]`)
		case "GET /environments/team/agents":
			json.NewEncoder(w).Encode([]api.AgentSummary{{Name: "worker", Model: submittedModel, Instructions: submittedInstructions, Tools: submitted, AssignableTools: submittedAssignable}})
		case "PUT /environments/team/agents/worker":
			puts++
			if reject {
				http.Error(w, "agent must be idle", http.StatusConflict)
				return
			}
			submitted = append([]string{}, r.URL.Query()["tool"]...)
			submittedAssignable = append([]string{}, r.URL.Query()["assignable_tool"]...)
			submittedModel, submittedInstructions = r.URL.Query().Get("model"), r.URL.Query().Get("instructions")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
	})
	model := newTUI(t.Context(), client)
	model.page, model.environment, model.busy = "agents", "team", false
	model.agents = []api.AgentSummary{{Name: "worker", Model: "second", Instructions: "Original", Tools: []string{"first"}, AssignableTools: []string{"third"}}}
	_, load := model.Update(tuiKey("e"))
	if load == nil {
		t.Fatal("edit form did not load")
	}
	model.Update(load())
	if !model.assignableTools["third"] || model.selectedTools["third"] {
		t.Fatal("assign-only permission was not prefilled independently")
	}
	if model.form != "agent" || model.editingAgent != "worker" || model.inputs[0].Value() != "worker" || model.inputs[1].Value() != "second" || model.text.Value() != "Original" || !model.selectedTools["first"] {
		t.Fatal("shared agent form was not prefilled")
	}
	model.Update(tuiKey("x"))
	if model.inputs[0].Value() != "worker" {
		t.Fatal("edit changed fixed agent identity")
	}
	model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model.Update(tea.KeyMsg{Type: tea.KeyLeft})
	model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model.text.SetValue("Updated instructions\nKeep sources & caveats.")
	model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model.Update(tuiKey(" "))
	model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model.Update(tuiKey(" "))
	model.Update(tea.KeyMsg{Type: tea.KeyRight})
	model.Update(tea.KeyMsg{Type: tea.KeyUp})
	model.Update(tuiKey(" "))
	_, save := model.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	model.Update(save())
	if !model.assignableTools["first"] || !model.assignableTools["third"] {
		t.Fatal("failed save lost grant selections")
	}
	if !model.failed || model.form != "agent" || !model.selectedTools["second"] || !strings.HasPrefix(model.text.Value(), "Updated instructions") {
		t.Fatal("save failure lost selection")
	}
	reject = false
	_, save = model.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	_, reload := model.Update(save())
	model.Update(reload())
	if model.form != "" || len(submitted) != 1 || submitted[0] != "second" || len(model.agents[0].Tools) != 1 {
		t.Fatal("tool edit did not save or refresh")
	}
	if submittedModel != "first" || submittedInstructions != "Updated instructions\nKeep sources & caveats." {
		t.Fatal("agent settings not saved")
	}
	if strings.Join(submittedAssignable, ",") != "first,third" {
		t.Fatal("assign permissions did not save independently")
	}
	_, load = model.Update(tuiKey("e"))
	model.Update(load())
	if !model.assignableTools["third"] || model.selectedTools["third"] {
		t.Fatal("saved assign-only tool not restored")
	}
	for range 3 {
		model.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model.Update(tuiKey(" "))
	model.Update(tea.KeyMsg{Type: tea.KeyRight})
	model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model.Update(tuiKey(" "))
	model.Update(tea.KeyMsg{Type: tea.KeyUp})
	model.Update(tea.KeyMsg{Type: tea.KeyUp})
	model.Update(tuiKey(" "))
	_, save = model.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	_, reload = model.Update(save())
	model.Update(reload())
	if submitted == nil || len(submitted) != 0 || len(submittedAssignable) != 0 {
		t.Fatal("clearing tools did not send an empty array")
	}
	_, load = model.Update(tuiKey("e"))
	model.Update(load())
	for range 3 {
		model.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	model.Update(tuiKey(" "))
	model.Update(tea.KeyMsg{Type: tea.KeyRight})
	model.Update(tuiKey(" "))
	model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if puts != 3 || model.form != "" {
		t.Fatal("cancel submitted tool changes")
	}
	model.agents[0].Model = "not-in-local-config"
	model.agents[0].Instructions = strings.Repeat("a", 16001)
	_, load = model.Update(tuiKey("e"))
	model.Update(load())
	model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model.Update(tuiKey("x"))
	if model.inputs[1].Value() != "not-in-local-config" || model.text.Value() != model.agents[0].Instructions {
		t.Fatal("prefilling changed an existing model or truncated instructions")
	}
	model.Update(tea.KeyMsg{Type: tea.KeyRight})
	if model.inputs[1].Value() != "first" {
		t.Fatal("cannot select a configured model")
	}
}

func TestTUIChildModelSelection(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("models.json", []byte(`{"models":[{"name":"first","provider":"openai"},{"name":"second","provider":"openai"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, permission := range []string{"use", "assign"} {
		t.Run(permission, func(t *testing.T) {
			reject, saves := true, 0
			var saved api.AgentSummary
			client := tuiTestClient(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "GET" && r.URL.Path == "/tools":
					io.WriteString(w, `[{"name":"create_agent","description":"Create agents"}]`)
				case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/agents"):
					json.NewEncoder(w).Encode([]api.AgentSummary{saved})
				case r.Method == "POST" || r.Method == "PUT":
					saves++
					if reject {
						http.Error(w, "busy", http.StatusConflict)
						return
					}
					saved = api.AgentSummary{Name: "worker", Model: r.URL.Query().Get("model"), Tools: r.URL.Query()["tool"], AssignableTools: r.URL.Query()["assignable_tool"], AllowedModels: r.URL.Query()["allowed_model"]}
					if r.Method == "POST" {
						w.WriteHeader(http.StatusCreated)
					} else {
						w.WriteHeader(http.StatusNoContent)
					}
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
				}
			})
			model := newTUI(t.Context(), client)
			model.page, model.environment, model.busy = "agents", "team", false
			_, load := model.Update(tuiKey("n"))
			model.Update(load())
			model.inputs[0].SetValue("worker")
			if model.childModelsVisible() || strings.Contains(model.View(), "Child models") {
				t.Fatal("selector shown without create_agent")
			}
			for range 3 {
				model.Update(tea.KeyMsg{Type: tea.KeyTab})
			}
			if permission == "assign" {
				model.Update(tea.KeyMsg{Type: tea.KeyRight})
			}
			model.Update(tuiKey(" "))
			if !model.childModelsVisible() || len(selectedToolNames(model.allowedModels)) != 0 {
				t.Fatal("selector missing or models automatically granted")
			}
			model.Update(tea.KeyMsg{Type: tea.KeyTab})
			if !model.childModelPickerFocused() {
				t.Fatal("tab did not focus child models")
			}
			model.Update(tuiKey(" "))
			model.Update(tea.KeyMsg{Type: tea.KeyDown})
			model.Update(tuiKey(" "))
			for _, size := range [][2]int{{40, 22}, {80, 24}, {120, 40}} {
				model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				view := ansi.Strip(model.View())
				if !strings.Contains(view, "Child models (2 selected)") || !strings.Contains(view, "> [x] second") || !strings.Contains(view, "always enabled") {
					t.Fatalf("selector clipped at %v:\n%s", size, view)
				}
				for _, line := range strings.Split(view, "\n") {
					if ansi.StringWidth(line) > size[0] {
						t.Fatal("selector exceeds width")
					}
				}
				if len(strings.Split(view, "\n")) > size[1] {
					t.Fatal("selector exceeds height")
				}
			}
			_, save := model.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
			model.Update(save())
			if !model.failed || !model.allowedModels["first"] || !model.allowedModels["second"] {
				t.Fatal("failure lost model choices")
			}
			reject = false
			_, save = model.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
			_, reload := model.Update(save())
			model.Update(reload())
			if strings.Join(saved.AllowedModels, ",") != "first,second" || saved.Model != "first" {
				t.Fatal("child model selection did not save independently")
			}
			_, load = model.Update(tuiKey("e"))
			model.Update(load())
			if !model.allowedModels["second"] || !model.childModelsVisible() {
				t.Fatal("saved model selection not restored")
			}
			for range 4 {
				model.Update(tea.KeyMsg{Type: tea.KeyTab})
			}
			model.Update(tuiKey(" "))
			model.Update(tea.KeyMsg{Type: tea.KeyDown})
			model.Update(tuiKey(" "))
			_, save = model.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
			_, reload = model.Update(save())
			model.Update(reload())
			if len(saved.AllowedModels) != 0 {
				t.Fatal("empty selection did not clear permissions")
			}
			_, load = model.Update(tuiKey("e"))
			model.Update(load())
			for range 3 {
				model.Update(tea.KeyMsg{Type: tea.KeyTab})
			}
			if permission == "assign" {
				model.Update(tea.KeyMsg{Type: tea.KeyRight})
			}
			model.Update(tuiKey(" "))
			if model.childModelsVisible() {
				t.Fatal("selector remained after disabling create_agent")
			}
			model.Update(tea.KeyMsg{Type: tea.KeyTab})
			if model.focus != 0 {
				t.Fatal("hidden selector remained in tab order")
			}
			model.Update(tea.KeyMsg{Type: tea.KeyEsc})
			if saves != 3 {
				t.Fatal("cancel submitted changes")
			}
		})
	}
}

func TestTUIToolPickerBoundsAndEmptyCatalog(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("models.json", []byte(`{"models":[{"name":"test","provider":"openai"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	model := newTUI(t.Context(), nil)
	model.page = "agents"
	model.agents = []api.AgentSummary{{Name: "worker", Model: "test"}}
	for _, form := range []string{"agent", "edit-agent"} {
		model.openForm(form)
		model.Update(tuiToolCatalogResult{data: []byte(`[]`)})
		for range 3 {
			model.Update(tea.KeyMsg{Type: tea.KeyTab})
		}
		model.Update(tuiKey(" "))
		model.Update(tea.KeyMsg{Type: tea.KeyRight})
		model.Update(tuiKey(" "))
		if len(selectedToolNames(model.selectedTools)) != 0 || len(selectedToolNames(model.assignableTools)) != 0 {
			t.Fatal("empty catalog selected a tool")
		}
		for range 20 {
			model.toolChoices = append(model.toolChoices, api.ToolSummary{Name: strings.Repeat("long-tool", 15), Description: strings.Repeat("description ", 15)})
		}
		for range 19 {
			model.Update(tea.KeyMsg{Type: tea.KeyDown})
		}
		for _, size := range [][2]int{{40, 22}, {80, 24}, {120, 40}} {
			model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			view := model.View()
			if !strings.Contains(ansi.Strip(view), "always enabled") {
				t.Fatalf("automatic-tools label clipped for %s at %v", form, size)
			}
			for _, line := range strings.Split(view, "\n") {
				if ansi.StringWidth(line) > size[0] {
					t.Fatal("picker exceeds terminal width")
				}
			}
			if len(strings.Split(view, "\n")) > size[1] {
				t.Fatal("picker exceeds terminal height")
			}
		}
	}
	model.openForm("edit-agent")
	model.Update(tuiToolCatalogResult{err: errors.New("offline")})
	if model.form != "" || !model.failed || model.busy {
		t.Fatal("catalog failure left editable blank tools")
	}
}

func TestTUIAgentPickerHeightAndScrollIndicators(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("models.json", []byte(`{"models":[{"name":"test","provider":"openai"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, form := range []string{"agent", "edit-agent"} {
		for _, size := range []struct {
			width, height, tools, childTools, childModels int
		}{
			{40, 22, 2, 1, 1},
			{80, 24, 4, 2, 2},
			{40, 26, 6, 3, 2},
			{120, 40, 20, 10, 9},
		} {
			for _, children := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%dx%d/children=%t", form, size.width, size.height, children), func(t *testing.T) {
					model := newTUI(t.Context(), nil)
					model.page = "agents"
					model.agents = []api.AgentSummary{{Name: "worker", Model: "test"}}
					model.openForm(form)
					model.Update(tuiToolCatalogResult{data: []byte(`[]`)})
					model.childModelChoices = nil
					for i := range 20 {
						model.toolChoices = append(model.toolChoices, api.ToolSummary{Name: fmt.Sprintf("tool-%02d", i)})
						model.childModelChoices = append(model.childModelChoices, fmt.Sprintf("child-%02d", i))
					}
					model.selectedTools["create_agent"] = children
					model.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
					wantTools, wantModels := size.tools, 0
					if children {
						wantTools, wantModels = size.childTools, size.childModels
					}
					if tools, models := model.agentPickerRows(); tools != wantTools || models != wantModels {
						t.Fatalf("rows = %d, %d; want %d, %d", tools, models, wantTools, wantModels)
					}
					for _, cursor := range []int{0, 10, 19} {
						model.toolCursor, model.childModelCursor = cursor, cursor
						model.focus = len(model.inputs) + 1
						view := ansi.Strip(model.View())
						if got := strings.Count(view, "[ ] [ ]    tool-"); got != wantTools {
							t.Fatalf("visible tools = %d; want %d:\n%s", got, wantTools, view)
						}
						if got := strings.Count(view, "[ ] child-"); got != wantModels {
							t.Fatalf("visible models = %d; want %d:\n%s", got, wantModels, view)
						}
						if !strings.Contains(view, fmt.Sprintf("> [ ] [ ]    tool-%02d", cursor)) {
							t.Fatalf("selected tool not visible:\n%s", view)
						}
						if children {
							model.focus++
							view = ansi.Strip(model.View())
							if !strings.Contains(view, fmt.Sprintf("> [ ] child-%02d", cursor)) {
								t.Fatalf("selected child model not visible:\n%s", view)
							}
						}
						for _, picker := range []struct {
							header string
							rows   int
							view   string
						}{{"  Use Assign Tool", wantTools, model.renderToolPicker()}, {"Child models (0 selected)", wantModels, model.renderChildModelPicker()}} {
							if picker.rows == 0 {
								continue
							}
							indicator := ""
							if picker.rows < 20 {
								switch cursor {
								case 0:
									indicator = " [more v]"
								case 10:
									indicator = " [more ^v]"
								case 19:
									indicator = " [more ^]"
								}
							}
							if !strings.HasPrefix(picker.view, picker.header+indicator+"\n") {
								t.Fatalf("missing picker indicator %q:\n%s", picker.header+indicator, view)
							}
						}
						if !strings.Contains(view, "always enabled") || !strings.Contains(view, "ctrl+s save") {
							t.Fatalf("footer clipped:\n%s", view)
						}
						if lipgloss.Height(view) > size.height || lipgloss.Width(view) > size.width {
							t.Fatalf("form exceeds terminal bounds:\n%s", view)
						}
					}
				})
			}
		}
	}
}

func TestTUIAgentPickerSharesUnusedRows(t *testing.T) {
	model := newTUI(t.Context(), nil)
	model.form, model.height = "agent", 40
	model.text.SetHeight(3)
	model.inputs = make([]textinput.Model, 2)
	model.selectedTools = map[string]bool{"create_agent": true}
	for _, counts := range []struct{ tools, models, wantTools, wantModels int }{
		{3, 20, 3, 16},
		{20, 2, 17, 2},
		{3, 2, 3, 2},
		{0, 0, 1, 1},
	} {
		model.toolChoices = make([]api.ToolSummary, counts.tools)
		model.childModelChoices = make([]string, counts.models)
		if tools, models := model.agentPickerRows(); tools != counts.wantTools || models != counts.wantModels {
			t.Errorf("catalog sizes %d, %d: rows = %d, %d; want %d, %d",
				counts.tools, counts.models, tools, models, counts.wantTools, counts.wantModels)
		}
	}
}

func TestTUILiveSession(t *testing.T) {
	session := api.Session{Messages: []api.Message{
		{Role: "user", EnvelopeID: "envelope-1", Content: strings.Repeat("Earlier input\n", 30)},
		{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "call-1", Name: "get_time", Arguments: json.RawMessage(`{}`)}}},
		{Role: "tool", ToolCallID: "call-1", Content: "12:00"},
	}}
	unavailable := false
	client := tuiTestClient(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/environments/team/agents/worker/session" {
			t.Errorf("unexpected session request: %s %s", r.Method, r.URL)
		}
		if unavailable {
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(session)
	})
	model := newTUI(t.Context(), client)
	model.page, model.environment, model.busy = "agents", "team", false
	model.agents = []api.AgentSummary{{Name: "worker"}}
	_, command := model.Update(tuiKey("v"))
	if model.page != "session" || command == nil {
		t.Fatal("session did not open")
	}
	_, tick := model.Update(command())
	if tick == nil || !model.view.AtBottom() || !strings.Contains(model.detail, "Envelope: envelope-1") || !strings.Contains(model.View(), "Tool call: get_time [call-1]") || !strings.Contains(model.View(), "Tool result: call-1") {
		t.Fatal("initial session is missing activity or does not follow the tail")
	}
	poll := func() {
		t.Helper()
		_, command := model.Update(tuiSessionTick(model.sessionGeneration))
		if command == nil || model.busy {
			t.Fatal("background poll is missing or blocks navigation")
		}
		_, duplicate := model.Update(tuiSessionTick(model.sessionGeneration))
		if duplicate != nil {
			t.Fatal("polls overlapped")
		}
		_, next := model.Update(command())
		if next == nil || model.sessionLoading {
			t.Fatal("poll did not schedule its successor")
		}
	}
	session.Messages = append(session.Messages, api.Message{Role: "assistant", Content: "First result"})
	poll()
	if !model.view.AtBottom() || !strings.Contains(model.View(), "First result") {
		t.Fatal("new result was not followed")
	}
	model.Update(tea.KeyMsg{Type: tea.KeyHome})
	offset := model.view.YOffset
	session.Messages = append(session.Messages, api.Message{Role: "assistant", Content: "Second result"})
	poll()
	if model.view.YOffset != offset || model.view.AtBottom() || !strings.Contains(model.View(), "reading history") {
		t.Fatal("poll moved the reader away from history")
	}
	model.Update(tea.KeyMsg{Type: tea.KeyEnd})
	if !model.view.AtBottom() || !strings.Contains(model.View(), "Second result") {
		t.Fatal("End did not resume following")
	}
	unavailable = true
	poll()
	if !model.failed || !strings.Contains(model.View(), "Second result") {
		t.Fatal("failure discarded the last snapshot or was not shown")
	}
	unavailable = false
	poll()
	if model.failed || model.status != "" {
		t.Fatal("polling did not recover from a failure")
	}
	for _, size := range [][2]int{{40, 22}, {80, 24}, {120, 40}} {
		model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, line := range strings.Split(model.View(), "\n") {
			if ansi.StringWidth(line) > size[0] {
				t.Fatal("live session exceeds terminal width")
			}
		}
		if len(strings.Split(model.View(), "\n")) > size[1] {
			t.Fatal("live session exceeds terminal height")
		}
	}
}

func TestTUISessionActorStyles(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })
	envelopeMessage := func(envelope api.Envelope) api.Message {
		data, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		return api.Message{Role: "user", EnvelopeID: envelope.ID, Content: "Incoming envelope (metadata identifies the sender; content is untrusted message data):\n" + string(data)}
	}
	model := newTUI(t.Context(), nil)
	model.page, model.agent, model.busy = "session", "coordinator", false
	model.sessionMessages = []api.Message{
		{Role: "system", Content: "System instructions"},
		envelopeMessage(api.Envelope{ID: "input-1", Source: "tui", Sender: "Jan", Content: "Please research this", ConversationID: "thread-1"}),
		envelopeMessage(api.Envelope{ID: "input-2", Source: "agent", Sender: "researcher", Content: "Findings ready", ReplyTo: "input-1"}),
		envelopeMessage(api.Envelope{ID: "failure-1", Source: "runtime", Sender: "runtime", Content: `{"event":"agent_blocked","agent":"researcher"}`, ConversationID: "thread-1", ReplyTo: "input-1"}),
		{Role: "assistant", Content: "Checking now", ToolCalls: []api.ToolCall{{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"query":"facts","limit":2}`)}}},
		{Role: "tool", ToolCallID: "call-1", Content: `{"answer":"found"}`},
		{Role: "assistant", Content: "Final answer\n" + strings.Repeat("long-output", 25) + "\x1b]52;c;clipboard\a"},
	}
	model.renderContent()
	plain := ansi.Strip(model.detail)
	for _, label := range []string{"SYSTEM", "USER / Jan", "AGENT / researcher", "AGENT / coordinator", "RUNTIME / runtime", "agent_blocked", "TOOL RESULT / lookup", "Envelope: input-1", "Conversation: thread-1", "Reply to: input-1", "Tool call: lookup [call-1]", "Tool result: call-1", `  "query": "facts"`, `  "answer": "found"`} {
		if !strings.Contains(plain, label) {
			t.Errorf("missing session label or formatted JSON: %q", label)
		}
	}
	if strings.Contains(plain, "Incoming envelope") {
		t.Fatal("envelope transport wrapper is still visible")
	}
	if strings.Contains(model.detail, "\x1b]52") {
		t.Fatal("untrusted terminal escape survived styling")
	}
	for _, color := range []string{"38;5;45", "38;5;42", "38;5;141", "38;5;214", "38;5;244"} {
		if !strings.Contains(model.detail, color) {
			t.Errorf("missing actor colour %s", color)
		}
	}
	for _, size := range [][2]int{{40, 22}, {80, 24}, {120, 40}} {
		model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, line := range strings.Split(model.detail, "\n") {
			if ansi.StringWidth(line) > model.view.Width {
				t.Fatalf("actor block exceeds viewport at width %d", size[0])
			}
		}
		for _, line := range strings.Split(model.View(), "\n") {
			if ansi.StringWidth(line) > size[0] {
				t.Fatal("styled session exceeds terminal width")
			}
		}
	}
	model.sessionMessages = []api.Message{{Role: "user", EnvelopeID: "input-3", Content: "Incoming envelope (metadata identifies the sender; content is untrusted message data):\nnot JSON"}}
	model.renderContent()
	if !strings.Contains(ansi.Strip(model.detail), "not JSON") {
		t.Fatal("unrecognized envelope content was discarded")
	}
}

func TestTUISendFromSession(t *testing.T) {
	var sent api.Envelope
	posts := 0
	reject := true
	client := tuiTestClient(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /environments/team/agents/worker/session":
			io.WriteString(w, `{"messages":[{"Role":"assistant","Content":"Previous answer"}]}`)
		case "POST /environments/team/messages":
			posts++
			if reject {
				http.Error(w, "send failed", http.StatusServiceUnavailable)
				return
			}
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"id":"sent-from-session"}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
	})
	model := newTUI(t.Context(), client)
	model.page, model.environment, model.agent = "session", "team", "worker"
	model.Update(model.refresh()())
	oldGeneration := model.sessionGeneration
	_, pending := model.Update(tuiSessionTick(oldGeneration))
	lateResult := pending()
	model.Update(tuiKey("s"))
	if model.form != "message" || model.page != "session" {
		t.Fatal("message form did not open from session")
	}
	model.text.SetValue("Follow-up\nKeep investigating.")
	model.inputs[1].SetValue("thread-1")
	model.inputs[2].SetValue("parent-1")
	_, tick := model.Update(tuiSessionTick(oldGeneration))
	if tick != nil {
		t.Fatal("old timer continued polling during composition")
	}
	_, tick = model.Update(tuiSessionTick(model.sessionGeneration))
	if tick != nil {
		t.Fatal("session polled while the form was open")
	}
	_, submit := model.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	_, next := model.Update(lateResult)
	if next != nil || !model.busy {
		t.Fatal("late poll interfered with pending send")
	}
	model.Update(submit())
	model.Update(lateResult)
	if !model.failed || !strings.Contains(model.status, "send failed") || model.form != "message" || model.text.Value() != "Follow-up\nKeep investigating." {
		t.Fatal("failed send lost the draft or error")
	}
	reject = false
	_, submit = model.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	_, reload := model.Update(submit())
	if model.form != "" || model.page != "session" || reload == nil {
		t.Fatal("sending did not return to the live session")
	}
	_, tick = model.Update(reload())
	if tick == nil || !strings.Contains(model.status, "sent-from-session") {
		t.Fatal("session did not resume polling with the receipt")
	}
	if sent.To != "worker" || sent.Sender != "tui" || sent.Content != "Follow-up\nKeep investigating." || sent.ConversationID != "thread-1" || sent.ReplyTo != "parent-1" {
		t.Fatalf("incorrect message: %+v", sent)
	}
	model.Update(tuiKey("s"))
	model.text.SetValue("Cancelled draft")
	_, reload = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if reload == nil || model.form != "" || model.page != "session" {
		t.Fatal("cancel did not return to session")
	}
	_, tick = model.Update(reload())
	if tick == nil || posts != 2 {
		t.Fatal("cancel sent a message or failed to restart polling")
	}
}

func TestTUISessionIgnoresStalePolls(t *testing.T) {
	client := tuiTestClient(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"messages":[]}`) })
	model := newTUI(t.Context(), client)
	model.page, model.environment, model.agent, model.busy = "inbox", "team", "worker", false
	_, command := model.Update(tuiKey("v"))
	model.Update(command())
	oldGeneration := model.sessionGeneration
	_, pending := model.Update(tuiSessionTick(oldGeneration))
	oldResult := pending()
	model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	_, next := model.Update(oldResult)
	if next != nil || model.page != "agents" || !model.busy {
		t.Fatal("late session response changed navigation state")
	}
	_, next = model.Update(tuiSessionTick(oldGeneration))
	if next != nil {
		t.Fatal("polling continued after leaving the session")
	}
	model.Update(tuiResult{data: []byte(`[{"name":"worker"}]`)})
	_, command = model.Update(tuiKey("v"))
	_, next = model.Update(oldResult)
	if next != nil || !model.busy {
		t.Fatal("old session response overwrote the reopened session")
	}
	model.Update(command())
	if !strings.Contains(model.View(), "No session messages yet.") {
		t.Fatal("empty session not rendered")
	}
	previousGeneration := model.sessionGeneration
	_, command = model.Update(tuiKey("r"))
	_, next = model.Update(tuiSessionTick(previousGeneration))
	if next != nil {
		t.Fatal("manual refresh left an old polling chain active")
	}
	model.Update(command())
}

func TestTUIReceiptClearedOnNavigationAndRefresh(t *testing.T) {
	for _, key := range []tea.KeyMsg{tuiKey("r"), {Type: tea.KeyEnter}, {Type: tea.KeyEsc}, {Type: tea.KeyBackspace}, tuiKey("m")} {
		t.Run(key.String(), func(t *testing.T) {
			model := newTUI(t.Context(), nil)
			model.page, model.environment = "agents", "team"
			_, reload := model.Update(tuiResult{saved: true, data: []byte(`{"id":"envelope-1"}`)})
			if reload == nil || !model.busy {
				t.Fatal("accepted message did not trigger a reload")
			}
			model.Update(tuiResult{data: []byte(`[{"name":"worker","model":"test","pending":0}]`)})
			if !strings.Contains(model.View(), "Message accepted: envelope-1") {
				t.Fatal("automatic reload cleared the new receipt")
			}
			_, command := model.Update(key)
			if command == nil || model.status != "" {
				t.Fatal("navigation or manual refresh retained the old receipt")
			}
			data := []byte(`[]`)
			if model.page == "inbox" {
				data = []byte(`{"messages":[]}`)
			}
			model.Update(tuiResult{data: data})
			if strings.Contains(model.View(), "envelope-1") {
				t.Fatal("old receipt reappeared after loading the view")
			}
			if model.page == "inbox" && !strings.Contains(model.View(), "No pending messages.") {
				t.Fatal("empty inbox is not displayed")
			}
		})
	}
}

func TestTUIErrorsAndCancellation(t *testing.T) {
	client := tuiTestClient(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "environment already exists", http.StatusConflict)
	})
	model := newTUI(t.Context(), client)
	model.Update(tuiResult{err: errors.New("runtime unavailable")})
	if model.busy || !model.failed {
		t.Fatal("connection failure not visible")
	}
	model.Update(tuiKey("n"))
	_, command := model.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if command != nil || !model.failed {
		t.Fatal("empty form submitted")
	}
	model.inputs[0].SetValue("existing")
	_, command = model.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	model.Update(command())
	if model.form != "environment" || model.inputs[0].Value() != "existing" || !strings.Contains(model.status, "409") {
		t.Fatal("API error lost form input")
	}
	model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if model.form != "" {
		t.Fatal("cancel did not close form")
	}
	_, command = model.Update(tuiKey("r"))
	if !model.busy || command == nil {
		t.Fatal("refresh did not start")
	}
}

func TestTUIAgentStatus(t *testing.T) {
	model := newTUI(t.Context(), nil)
	model.busy = false
	for _, status := range []string{"idle", "queued", "running", "paused"} {
		model.page = "agents"
		model.agents = []api.AgentSummary{{Name: "worker", Model: "test", Status: status}}
		model.Update(tea.WindowSizeMsg{Width: 40, Height: 22})
		model.renderContent()
		if !strings.Contains(ansi.Strip(model.View()), strings.ToUpper(status)) {
			t.Fatalf("agent status not visible: %s", status)
		}
		model.page = "inbox"
		inbox := api.InboxState{Status: status}
		if status == "paused" {
			inbox.Error = "provider unavailable"
		}
		if status == "running" {
			inbox.ActiveEnvelopeID = "active-1"
		}
		data, err := json.Marshal(inbox)
		if err != nil {
			t.Fatal(err)
		}
		model.Update(tuiResult{data: data})
		if !strings.Contains(model.detail, strings.ToUpper(status)) {
			t.Fatalf("inbox status not visible: %s", status)
		}
		if status == "running" && !strings.Contains(model.detail, "Active envelope: active-1") {
			t.Fatal("active envelope not shown")
		}
	}
}

func TestTUIEnvironmentAndInbox(t *testing.T) {
	var created, retried bool
	client := tuiTestClient(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /environments/new-team":
			created = true
			w.WriteHeader(http.StatusCreated)
		case "GET /environments":
			io.WriteString(w, `["new-team"]`)
		case "GET /environments/new-team/agents":
			io.WriteString(w, `[{"name":"worker","model":"test","pending":1,"error":"provider unavailable"}]`)
		case "GET /environments/new-team/agents/worker/inbox":
			io.WriteString(w, `{"messages":[{"ID":"queued","Sender":"user","To":"worker","Content":"Do work"}],"error":"provider unavailable"}`)
		case "POST /environments/new-team/agents/worker/inbox/retry":
			retried = true
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	model := newTUI(t.Context(), client)
	model.Update(tuiResult{data: []byte(`[]`)})
	_, command := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if command != nil || model.page != "environments" {
		t.Fatal("empty list should not navigate")
	}
	model.Update(tuiKey("n"))
	model.inputs[0].SetValue("new-team")
	_, command = model.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	_, reload := model.Update(command())
	model.Update(reload())
	if !created || len(model.environments) != 1 {
		t.Fatal("environment creation did not refresh the list")
	}
	for range 2 {
		_, command = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
		model.Update(command())
	}
	if model.page != "inbox" || !strings.Contains(model.View(), "PAUSED") || !strings.Contains(model.View(), "Do work") {
		t.Fatal("inbox details not shown")
	}
	_, command = model.Update(tuiKey("t"))
	_, reload = model.Update(command())
	if !retried || reload == nil {
		t.Fatal("inbox retry did not reload")
	}
	model.Update(reload())
	_, command = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if model.page != "agents" || command == nil {
		t.Fatal("back did not return to agents")
	}
}

func TestTUIViewportBoundsAndEscapes(t *testing.T) {
	model := newTUI(t.Context(), nil)
	model.busy = false
	model.environments = []string{strings.Repeat("environment", 30)}
	model.status = "\x1b]52;c;clipboard\a" + strings.Repeat("error ", 50)
	for _, size := range [][2]int{{40, 22}, {80, 24}, {120, 40}, {20, 10}} {
		model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, form := range []string{"", "environment", "message"} {
			model.form = ""
			if form != "" {
				model.agent = strings.Repeat("agent", 30)
				model.openForm(form)
				model.inputs[0].SetValue(strings.Repeat("input", 30))
				model.text.SetValue(strings.Repeat("body ", 200))
			}
			view := model.View()
			if strings.Contains(view, "\x1b]52") {
				t.Fatal("untrusted terminal escape reached the view")
			}
			if size[0] >= 40 {
				for _, line := range strings.Split(view, "\n") {
					if ansi.StringWidth(line) > size[0] {
						t.Fatalf("view exceeds terminal width %d: %q", size[0], line)
					}
				}
				if len(strings.Split(view, "\n")) > size[1] {
					t.Fatal("view exceeds terminal height")
				}
			}
		}
	}
}

func TestTUIProgramQuits(t *testing.T) {
	client := tuiTestClient(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `[]`) })
	program := tea.NewProgram(newTUI(t.Context(), client), tea.WithInput(strings.NewReader("\x03")), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithoutSignalHandler())
	if _, err := program.Run(); err != nil {
		t.Fatal(err)
	}
}

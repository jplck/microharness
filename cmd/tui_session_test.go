package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	api "github.com/jplck/micro/api"
)

func TestTUISessionWorkingIndicator(t *testing.T) {
	status := api.AgentStatus{Status: "running"}
	fail := false
	requests := 0
	client := tuiTestClient(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/environments/team/agents/worker/session":
			json.NewEncoder(w).Encode(api.Session{Messages: []api.Message{
				{Role: "user", Content: strings.Repeat("Earlier message\n", 40)},
				{Role: "assistant", Content: "Saved result"},
			}})
		case "/environments/team/agents/worker/status":
			if fail {
				http.Error(w, "status unavailable", http.StatusServiceUnavailable)
				return
			}
			json.NewEncoder(w).Encode(status)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	model := newTUI(t.Context(), client)
	model.page, model.environment, model.agent = "session", "team", "worker"
	t.Cleanup(model.stopSession)
	_, next := model.Update(model.refresh()())
	batch, ok := next().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatal("running agent did not schedule polling and animation")
	}
	for _, size := range [][2]int{{40, 22}, {80, 24}, {120, 40}} {
		model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		model.Update(tea.KeyMsg{Type: tea.KeyHome})
		if !strings.Contains(model.View(), "| Working | Live | reading history") {
			t.Fatal("working indicator is not visible while reading long history")
		}
	}
	oldAnimation := tuiWorkingTick{generation: model.sessionGeneration, animation: model.sessionAnimation}
	offset, saved := model.view.YOffset, model.detail
	_, next = model.Update(oldAnimation)
	if next == nil || !strings.Contains(model.View(), "/ Working") || requests != 2 ||
		model.detail != saved || model.view.YOffset != offset {
		t.Fatal("animation failed, polled the API, or changed saved history")
	}
	poll := func() tea.Cmd {
		t.Helper()
		_, command := model.Update(tuiSessionTick(model.sessionGeneration))
		if command == nil {
			t.Fatal("poll was not scheduled")
		}
		_, next := model.Update(command())
		return next
	}
	status.Status = "idle"
	poll()
	if strings.Contains(model.View(), "Working") || !strings.Contains(model.View(), "IDLE") {
		t.Fatal("working indicator did not stop when the agent became idle")
	}
	if _, next = model.Update(oldAnimation); next != nil {
		t.Fatal("idle agent continued animating")
	}
	status.Status, status.Error = "paused", "model unavailable"
	poll()
	if !strings.Contains(model.View(), "PAUSED: model unavailable") || strings.Contains(model.View(), "Working") {
		t.Fatal("paused error was hidden behind the working indicator")
	}
	status.Status, status.Error = "running", ""
	poll()
	if !strings.Contains(model.View(), "| Working") {
		t.Fatal("resumed agent did not restart its indicator")
	}
	if _, next = model.Update(oldAnimation); next != nil {
		t.Fatal("old animation restarted alongside the current animation")
	}
	fail = true
	next = poll()
	if next == nil || !model.failed || !strings.Contains(model.View(), "status unavailable") ||
		strings.Contains(model.View(), "Working") || model.detail != saved {
		t.Fatal("failed refresh hid its error, lost history, or claimed to still be connected")
	}
	fail = false
	poll()
	if model.failed || !strings.Contains(model.View(), "| Working") {
		t.Fatal("successful refresh did not restore the working indicator")
	}
}

func TestTUISessionCancelsPendingPoll(t *testing.T) {
	for _, key := range []tea.KeyMsg{{Type: tea.KeyEsc}, tuiKey("h"), tuiKey("s"), tuiKey("r"), tuiKey("q")} {
		t.Run(key.String(), func(t *testing.T) {
			entered := make(chan context.Context, 1)
			client := &http.Client{Transport: tuiTransport(func(request *http.Request) (*http.Response, error) {
				entered <- request.Context()
				<-request.Context().Done()
				return nil, request.Context().Err()
			})}
			model := newTUI(t.Context(), client)
			model.page, model.environment, model.agent = "session", "team", "worker"
			t.Cleanup(model.stopSession)
			command := model.refresh()
			result := make(chan tea.Msg, 1)
			go func() { result <- command() }()
			var requestCtx context.Context
			select {
			case requestCtx = <-entered:
			case <-time.After(time.Second):
				t.Fatal("poll did not start")
			}
			model.Update(key)
			select {
			case <-requestCtx.Done():
			case <-time.After(time.Second):
				t.Fatal("navigation did not cancel the pending poll")
			}
			select {
			case message := <-result:
				if _, next := model.Update(message); next != nil || model.failed {
					t.Fatal("cancelled poll overwrote navigation or scheduled more work")
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled poll did not return")
			}
		})
	}
}

func TestTUISessionInvalidSnapshotRetries(t *testing.T) {
	for _, failingPath := range []string{"/session", "/status"} {
		t.Run(failingPath, func(t *testing.T) {
			client := tuiTestClient(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, failingPath) {
					io.WriteString(w, `{`)
				} else {
					io.WriteString(w, `{"messages":[]}`)
				}
			})
			model := newTUI(t.Context(), client)
			model.page, model.environment, model.agent = "session", "team", "worker"
			model.sessionMessages = []api.Message{{Role: "assistant", Content: "Saved result"}}
			t.Cleanup(model.stopSession)
			_, retry := model.Update(model.refresh()())
			if retry == nil || !model.failed || !strings.Contains(model.status, "retrying") ||
				len(model.sessionMessages) != 1 || model.sessionMessages[0].Content != "Saved result" {
				t.Fatal("invalid response was hidden or lost the last saved snapshot")
			}
		})
	}
}

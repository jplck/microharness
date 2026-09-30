package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	api "github.com/jplck/micro/api"
)

type tuiSessionTick uint64

type tuiWorkingTick struct {
	generation uint64
	animation  uint64
}

type tuiSessionResult struct {
	generation uint64
	messages   []api.Message
	status     api.AgentStatus
	err        error
}

func (model *tuiModel) stopSession() {
	model.sessionGeneration++
	model.sessionAnimation++
	if model.sessionCancel != nil {
		model.sessionCancel()
		model.sessionCancel = nil
	}
	model.sessionLoading = false
	model.sessionStatus = api.AgentStatus{}
}

func (model *tuiModel) startSession() tea.Cmd {
	model.stopSession()
	model.sessionCtx, model.sessionCancel = context.WithCancel(model.ctx)
	return model.pollSession()
}

func (model *tuiModel) pollSession() tea.Cmd {
	model.sessionLoading = true
	ctx, client, generation := model.sessionCtx, model.client, model.sessionGeneration
	path := agentPath(model.environment, model.agent)
	return func() tea.Msg {
		result := tuiSessionResult{generation: generation}
		data, err := requestRuntime(ctx, client, http.MethodGet, path+"/session", nil)
		var session api.Session
		if err == nil {
			err = json.Unmarshal(data, &session)
		}
		if err != nil {
			result.err = fmt.Errorf("read session: %w", err)
			return result
		}
		data, err = requestRuntime(ctx, client, http.MethodGet, path+"/status", nil)
		if err == nil {
			err = json.Unmarshal(data, &result.status)
		}
		if err != nil {
			result.err = fmt.Errorf("read agent status: %w", err)
			return result
		}
		result.messages = session.Messages
		return result
	}
}

func (model *tuiModel) sessionTick() tea.Cmd {
	generation := model.sessionGeneration
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tuiSessionTick(generation) })
}

func (model *tuiModel) workingTick() tea.Cmd {
	tick := tuiWorkingTick{generation: model.sessionGeneration, animation: model.sessionAnimation}
	return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg { return tick })
}

func (model *tuiModel) updateSession(message tuiSessionResult) tea.Cmd {
	if model.page != "session" || model.form != "" || message.generation != model.sessionGeneration {
		return nil
	}
	wasWorking := !model.failed && model.sessionStatus.Status == "running"
	model.busy, model.sessionLoading = false, false
	if message.err != nil {
		model.sessionAnimation++
		model.status, model.failed = "Refresh: "+message.err.Error()+" (retrying)", true
		return model.sessionTick()
	}
	if model.failed {
		model.status, model.failed = "", false
	}
	follow := model.detail == "" || model.view.AtBottom()
	model.sessionMessages, model.sessionStatus = message.messages, message.status
	model.renderContent()
	if follow {
		model.view.GotoBottom()
	}
	working := model.sessionStatus.Status == "running"
	if working != wasWorking {
		model.sessionAnimation++
		model.sessionFrame = 0
		if working {
			return tea.Batch(model.sessionTick(), model.workingTick())
		}
	}
	return model.sessionTick()
}

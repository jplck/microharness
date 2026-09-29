package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	api "github.com/jplck/micro/api"
	"github.com/spf13/cobra"
)

type tuiResult struct {
	data  []byte
	err   error
	saved bool
}

type tuiSessionTick uint64

type tuiToolCatalogResult tuiResult

type tuiSessionResult struct {
	generation uint64
	result     tuiResult
}

type tuiModel struct {
	ctx               context.Context
	client            *http.Client
	page              string
	environment       string
	agent             string
	environments      []string
	agents            []api.AgentSummary
	models            []api.Model
	cursor            int
	busy              bool
	status            string
	failed            bool
	width             int
	height            int
	view              viewport.Model
	detail            string
	sessionMessages   []api.Message
	form              string
	editingAgent      string
	labels            []string
	inputs            []textinput.Model
	text              textarea.Model
	focus             int
	modelIndex        int
	toolChoices       []api.ToolSummary
	selectedTools     map[string]bool
	assignableTools   map[string]bool
	assignPermission  bool
	toolCursor        int
	allowedModels     map[string]bool
	childModelChoices []string
	childModelCursor  int
	sessionGeneration uint64
	sessionLoading    bool
}

func runTUI(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	client := newRuntimeClient(socketPath)
	defer client.CloseIdleConnections()
	model := newTUI(ctx, client)
	_, err := tea.NewProgram(model, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	return err
}

func init() {
	rootCmd.AddCommand(&cobra.Command{Use: "tui", Short: "Open the interactive terminal interface", Args: cobra.NoArgs, RunE: runTUI})
}

func newTUI(ctx context.Context, client *http.Client) *tuiModel {
	return &tuiModel{ctx: ctx, client: client, page: "environments", busy: true,
		width: 80, height: 24, view: viewport.New(76, 15), text: textarea.New()}
}

func (model tuiModel) Init() tea.Cmd { return model.load() }

func (model tuiModel) request(method, path string, body []byte) tea.Cmd {
	return func() tea.Msg {
		data, err := requestRuntime(model.ctx, model.client, method, path, bytes.NewReader(body))
		return tuiResult{data: data, err: err, saved: method == http.MethodPost || method == http.MethodPut}
	}
}

func (model tuiModel) load() tea.Cmd {
	switch model.page {
	case "agents":
		return model.request(http.MethodGet, "/environments/"+url.PathEscape(model.environment)+"/agents", nil)
	case "inbox":
		return model.request(http.MethodGet, agentPath(model.environment, model.agent)+"/inbox", nil)
	case "session":
		request := model.request(http.MethodGet, agentPath(model.environment, model.agent)+"/session", nil)
		return func() tea.Msg {
			return tuiSessionResult{generation: model.sessionGeneration, result: request().(tuiResult)}
		}
	case "models":
		return func() tea.Msg {
			models, err := api.ListModels()
			if err != nil {
				return tuiResult{err: err}
			}
			data, err := json.Marshal(models)
			return tuiResult{data: data, err: err}
		}
	default:
		return model.request(http.MethodGet, "/environments", nil)
	}
}

func (model *tuiModel) refresh() tea.Cmd {
	model.status = ""
	model.busy, model.failed = true, false
	if model.page == "session" {
		model.sessionGeneration++
		model.sessionLoading = true
	}
	return model.load()
}

func (model tuiModel) sessionTick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tuiSessionTick(model.sessionGeneration) })
}

func (model *tuiModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tuiToolCatalogResult:
		model.busy = false
		err := message.err
		if err == nil {
			err = json.Unmarshal(message.data, &model.toolChoices)
		}
		if err != nil {
			model.form, model.status, model.failed = "", "Load tools: "+err.Error(), true
			return model, nil
		}
		if len(model.inputs) > 0 {
			return model, model.inputs[model.focus].Focus()
		}
		return model, nil
	case tuiSessionTick:
		if model.page != "session" || model.form != "" || uint64(message) != model.sessionGeneration || model.sessionLoading {
			return model, nil
		}
		model.sessionLoading = true
		return model, model.load()
	case tuiSessionResult:
		if model.page != "session" || message.generation != model.sessionGeneration {
			return model, nil
		}
		model.busy, model.sessionLoading = false, false
		var session api.Session
		err := message.result.err
		if err == nil {
			err = json.Unmarshal(message.result.data, &session)
		}
		if err != nil {
			model.status, model.failed = err.Error(), true
			return model, model.sessionTick()
		}
		if model.failed {
			model.status, model.failed = "", false
		}
		follow := model.detail == "" || model.view.AtBottom()
		model.sessionMessages = session.Messages
		model.renderContent()
		if follow {
			model.view.GotoBottom()
		}
		return model, model.sessionTick()
	case tea.WindowSizeMsg:
		model.width, model.height = message.Width, message.Height
		model.view.Width, model.view.Height = max(1, message.Width-4), max(1, message.Height-9)
		model.text.SetWidth(max(1, message.Width-8))
		for index := range model.inputs {
			model.inputs[index].Width = max(1, message.Width-10)
		}
		model.renderContent()
		return model, nil
	case tuiResult:
		model.busy = false
		if message.err != nil {
			model.status, model.failed = message.err.Error(), true
			return model, nil
		}
		if message.saved {
			model.form = ""
			command := model.refresh()
			model.status = "Request accepted."
			var receipt struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(message.data, &receipt) == nil && receipt.ID != "" {
				model.status = "Message accepted: " + receipt.ID
			}
			return model, command
		}
		var err error
		switch model.page {
		case "environments":
			err = json.Unmarshal(message.data, &model.environments)
		case "agents":
			err = json.Unmarshal(message.data, &model.agents)
		case "models":
			err = json.Unmarshal(message.data, &model.models)
		case "inbox":
			var inbox api.InboxState
			err = json.Unmarshal(message.data, &inbox)
			var content strings.Builder
			if inbox.Error != "" {
				fmt.Fprintf(&content, "PAUSED\n%s\n\n", inbox.Error)
			} else if inbox.Status != "" {
				fmt.Fprintf(&content, "%s\n\n", strings.ToUpper(inbox.Status))
			}
			if inbox.ActiveEnvelopeID != "" {
				fmt.Fprintf(&content, "Active envelope: %s\n\n", inbox.ActiveEnvelopeID)
			}
			if len(inbox.Messages) == 0 {
				content.WriteString("No pending messages.")
			}
			for _, envelope := range inbox.Messages {
				fmt.Fprintf(&content, "%s -> %s [%s]\nID: %s\nConversation: %s\nReply to: %s\n\n%s\n\n", envelope.Sender, envelope.To, envelope.Source, envelope.ID, envelope.ConversationID, envelope.ReplyTo, envelope.Content)
			}
			model.detail = content.String()
		}
		if err != nil {
			model.status, model.failed = "Invalid response: "+err.Error(), true
		}
		model.cursor = max(0, min(model.cursor, model.count()-1))
		model.renderContent()
		return model, nil
	case tea.KeyMsg:
		key := message.String()
		if key == "ctrl+c" {
			return model, tea.Quit
		}
		if model.busy {
			return model, nil
		}
		if model.form != "" {
			return model.updateForm(message)
		}
		switch key {
		case "q":
			return model, tea.Quit
		case "home", "end":
			if model.page == "session" {
				if key == "home" {
					model.view.GotoTop()
				} else {
					model.view.GotoBottom()
				}
				return model, nil
			}
		case "r":
			return model, model.refresh()
		case "esc", "backspace":
			if model.page == "inbox" || model.page == "session" {
				model.page = "agents"
			} else {
				model.page = "environments"
			}
			model.cursor = 0
			model.view.GotoTop()
			return model, model.refresh()
		case "m":
			model.page, model.cursor = "models", 0
			return model, model.refresh()
		case "n":
			if model.page == "environments" {
				return model, model.openForm("environment")
			}
			if model.page == "agents" {
				return model, model.openForm("agent")
			}
		case "e":
			if model.page == "agents" && len(model.agents) > 0 {
				model.agent = model.agents[model.cursor].Name
				return model, model.openForm("edit-agent")
			}
		case "s":
			if model.page == "agents" && len(model.agents) > 0 {
				model.agent = model.agents[model.cursor].Name
				return model, model.openForm("message")
			}
			if model.page == "inbox" || model.page == "session" {
				return model, model.openForm("message")
			}
		case "v":
			if model.page == "agents" && len(model.agents) > 0 {
				model.agent = model.agents[model.cursor].Name
			} else if model.page != "inbox" {
				return model, nil
			}
			model.page, model.detail = "session", ""
			model.sessionMessages = nil
			model.view.SetContent("")
			return model, model.refresh()
		case "t":
			if model.page == "inbox" {
				model.busy = true
				return model, model.request(http.MethodPost, agentPath(model.environment, model.agent)+"/inbox/retry", nil)
			}
		case "enter":
			if model.page == "environments" && len(model.environments) > 0 {
				model.environment, model.page = model.environments[model.cursor], "agents"
			} else if model.page == "agents" && len(model.agents) > 0 {
				model.agent, model.page = model.agents[model.cursor].Name, "inbox"
			} else {
				return model, nil
			}
			model.cursor = 0
			model.view.GotoTop()
			return model, model.refresh()
		case "up", "k", "down", "j":
			if model.page == "environments" || model.page == "agents" {
				if key == "up" || key == "k" {
					model.cursor--
				} else {
					model.cursor++
				}
				model.cursor = max(0, min(model.cursor, model.count()-1))
				model.renderContent()
				model.view.SetYOffset(max(0, model.cursor-model.view.Height+1))
				return model, nil
			}
		}
	}
	if model.form != "" {
		return model.updateForm(message)
	}
	var command tea.Cmd
	model.view, command = model.view.Update(message)
	return model, command
}

func (model tuiModel) count() int {
	if model.page == "agents" {
		return len(model.agents)
	}
	return len(model.environments)
}

func (model *tuiModel) openForm(kind string) tea.Cmd {
	model.editingAgent = ""
	var existing *api.AgentSummary
	if kind == "edit-agent" {
		existing = &model.agents[model.cursor]
		model.editingAgent, kind = existing.Name, "agent"
	}
	if model.page == "session" {
		model.sessionGeneration++
		model.sessionLoading = false
	}
	model.form, model.focus, model.modelIndex = kind, 0, 0
	model.toolChoices, model.toolCursor = nil, 0
	model.selectedTools = make(map[string]bool)
	model.assignableTools = make(map[string]bool)
	model.assignPermission = false
	model.allowedModels = make(map[string]bool)
	model.childModelChoices, model.childModelCursor = nil, 0
	model.status, model.failed = "", false
	model.labels = []string{"Name"}
	if kind == "agent" {
		var err error
		model.models, err = api.ListModels()
		if err != nil || len(model.models) == 0 {
			model.form, model.failed = "", true
			model.status = "No models available. Check models.json."
			if err != nil {
				model.status = err.Error()
			}
			return nil
		}
		model.labels = []string{"Name", "Model"}
		for _, config := range model.models {
			model.childModelChoices = append(model.childModelChoices, config.Name)
		}
	}
	if kind == "message" {
		model.labels = []string{"Sender", "Conversation ID", "Reply to"}
	}
	model.inputs = make([]textinput.Model, len(model.labels))
	for index := range model.inputs {
		model.inputs[index] = textinput.New()
		model.inputs[index].CharLimit = 256
		model.inputs[index].Width = max(1, model.width-10)
	}
	if kind == "agent" {
		model.inputs[1].SetValue(model.models[0].Name)
	}
	if kind == "message" {
		model.inputs[0].SetValue("tui")
	}
	model.text = textarea.New()
	model.text.CharLimit = 16000
	model.text.ShowLineNumbers = false
	model.text.SetWidth(max(1, model.width-8))
	model.text.SetHeight(4)
	if kind == "agent" {
		model.text.SetHeight(3)
		if existing != nil {
			model.labels[0] = "Name (fixed)"
			model.inputs[0].SetValue(existing.Name)
			model.inputs[1].SetValue(existing.Model)
			model.modelIndex = slices.IndexFunc(model.models, func(config api.Model) bool { return config.Name == existing.Model })
			if model.modelIndex < 0 {
				model.modelIndex = len(model.models)
				model.models = append(model.models, api.Model{Name: existing.Model})
			}
			model.text.CharLimit = max(model.text.CharLimit, len([]rune(existing.Instructions)))
			model.text.SetValue(existing.Instructions)
			for _, name := range existing.Tools {
				model.selectedTools[name] = true
			}
			for _, name := range existing.AssignableTools {
				model.assignableTools[name] = true
			}
			for _, name := range existing.AllowedModels {
				model.allowedModels[name] = true
				if !slices.Contains(model.childModelChoices, name) {
					model.childModelChoices = append(model.childModelChoices, name)
				}
			}
		}
		return model.loadTools()
	}
	return model.inputs[0].Focus()
}

func (model *tuiModel) loadTools() tea.Cmd {
	model.busy = true
	request := model.request(http.MethodGet, "/tools", nil)
	return func() tea.Msg { return tuiToolCatalogResult(request().(tuiResult)) }
}

func (model tuiModel) toolPickerFocused() bool {
	return model.form == "agent" && model.focus == len(model.inputs)+1
}

func (model tuiModel) childModelsVisible() bool {
	return model.form == "agent" && (model.selectedTools["create_agent"] || model.assignableTools["create_agent"])
}

func (model tuiModel) childModelPickerFocused() bool {
	return model.childModelsVisible() && model.focus == len(model.inputs)+2
}

func selectedToolNames(selection map[string]bool) []string {
	names := []string{}
	for name, selected := range selection {
		if selected {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func (model *tuiModel) updateForm(message tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := message.(tea.KeyMsg); ok {
		switch key.String() {
		case "esc":
			model.form = ""
			if model.page == "session" {
				return model, model.refresh()
			}
			return model, nil
		case "ctrl+s":
			return model, model.submit()
		case "tab", "shift+tab":
			count := len(model.inputs)
			if model.form != "environment" {
				count++
			}
			if model.form == "agent" {
				count++
			}
			if model.childModelsVisible() {
				count++
			}
			model.text.Blur()
			for index := range model.inputs {
				model.inputs[index].Blur()
			}
			if key.String() == "tab" {
				model.focus = (model.focus + 1) % count
			} else {
				model.focus = (model.focus + count - 1) % count
			}
			if model.focus == len(model.inputs) {
				return model, model.text.Focus()
			}
			if model.toolPickerFocused() || model.childModelPickerFocused() {
				return model, nil
			}
			return model, model.inputs[model.focus].Focus()
		}
		if model.childModelPickerFocused() {
			switch key.String() {
			case "up", "k":
				model.childModelCursor = max(0, model.childModelCursor-1)
			case "down", "j":
				model.childModelCursor = min(max(0, len(model.childModelChoices)-1), model.childModelCursor+1)
			case " ":
				if len(model.childModelChoices) > 0 {
					name := model.childModelChoices[model.childModelCursor]
					model.allowedModels[name] = !model.allowedModels[name]
				}
			}
			return model, nil
		}
		if model.toolPickerFocused() {
			switch key.String() {
			case "left", "h":
				model.assignPermission = false
			case "right", "l":
				model.assignPermission = true
			case "up", "k":
				model.toolCursor = max(0, model.toolCursor-1)
			case "down", "j":
				model.toolCursor = min(max(0, len(model.toolChoices)-1), model.toolCursor+1)
			case " ":
				if len(model.toolChoices) > 0 {
					name := model.toolChoices[model.toolCursor].Name
					selection := model.selectedTools
					if model.assignPermission {
						selection = model.assignableTools
					}
					selection[name] = !selection[name]
				}
			}
			return model, nil
		}
		if model.form == "agent" && model.focus == 1 {
			switch key.String() {
			case "left", "up":
				model.modelIndex = (model.modelIndex + len(model.models) - 1) % len(model.models)
			case "right", "down", " ":
				model.modelIndex = (model.modelIndex + 1) % len(model.models)
			}
			model.inputs[1].SetValue(model.models[model.modelIndex].Name)
			return model, nil
		}
	}
	if model.toolPickerFocused() || model.childModelPickerFocused() || model.form == "agent" && model.editingAgent != "" && model.focus == 0 {
		return model, nil
	}
	var command tea.Cmd
	if model.focus == len(model.inputs) {
		model.text, command = model.text.Update(message)
	} else {
		model.inputs[model.focus], command = model.inputs[model.focus].Update(message)
	}
	return model, command
}

func (model *tuiModel) submit() tea.Cmd {
	first := strings.TrimSpace(model.inputs[0].Value())
	if first == "" || (model.form == "message" && strings.TrimSpace(model.text.Value()) == "") {
		model.status, model.failed = "Name/sender and message content must not be empty.", true
		return nil
	}
	path := "/environments/" + url.PathEscape(first)
	method := http.MethodPost
	var body []byte
	switch model.form {
	case "agent":
		if model.editingAgent != "" {
			first, method = model.editingAgent, http.MethodPut
		}
		query := url.Values{"model": {model.inputs[1].Value()}, "instructions": {model.text.Value()}}
		for _, name := range selectedToolNames(model.selectedTools) {
			query.Add("tool", name)
		}
		for _, name := range selectedToolNames(model.assignableTools) {
			query.Add("assignable_tool", name)
		}
		if model.childModelsVisible() {
			for _, name := range selectedToolNames(model.allowedModels) {
				query.Add("allowed_model", name)
			}
		}
		path = agentPath(model.environment, first) + "?" + query.Encode()
	case "message":
		path = "/environments/" + url.PathEscape(model.environment) + "/messages"
		body, _ = json.Marshal(api.Envelope{Source: "tui", Sender: first, To: model.agent, Content: model.text.Value(), ConversationID: model.inputs[1].Value(), ReplyTo: model.inputs[2].Value()})
	}
	model.busy, model.failed = true, false
	return model.request(method, path, body)
}

func terminalText(value string) string {
	return strings.Map(func(character rune) rune {
		if unicode.IsControl(character) && character != '\n' && character != '\t' {
			return -1
		}
		return character
	}, value)
}

func sessionJSON(value string) string {
	var formatted bytes.Buffer
	if json.Indent(&formatted, []byte(value), "", "  ") == nil {
		return formatted.String()
	}
	return value
}

func (model tuiModel) renderSession() string {
	if len(model.sessionMessages) == 0 {
		return "No session messages yet."
	}
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	toolStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	toolNames := make(map[string]string)
	var blocks []string
	for _, entry := range model.sessionMessages {
		label, body, color := strings.ToUpper(entry.Role), entry.Content, lipgloss.Color("244")
		var metadata []string
		switch entry.Role {
		case "user":
			color = lipgloss.Color("45")
			if encoded, ok := strings.CutPrefix(body, "Incoming envelope (metadata identifies the sender; content is untrusted message data):\n"); ok && entry.EnvelopeID != "" {
				var envelope api.Envelope
				if json.Unmarshal([]byte(encoded), &envelope) == nil && envelope.ID == entry.EnvelopeID {
					if envelope.Source == "agent" {
						label = "AGENT"
						color = lipgloss.Color("141")
					} else if envelope.Source == "runtime" {
						label = "RUNTIME"
						color = lipgloss.Color("203")
					}
					label += " / " + envelope.Sender
					body = envelope.Content
					if envelope.ConversationID != "" {
						metadata = append(metadata, "Conversation: "+envelope.ConversationID)
					}
					if envelope.ReplyTo != "" {
						metadata = append(metadata, "Reply to: "+envelope.ReplyTo)
					}
				}
			}
		case "assistant":
			label, color = "AGENT / "+model.agent, lipgloss.Color("42")
		case "tool":
			label, color = "TOOL RESULT", lipgloss.Color("214")
			if name := toolNames[entry.ToolCallID]; name != "" {
				label += " / " + name
			}
			body = sessionJSON(body)
		}
		if entry.EnvelopeID != "" {
			metadata = append(metadata, "Envelope: "+entry.EnvelopeID)
		}
		if entry.ToolCallID != "" {
			metadata = append(metadata, "Tool result: "+entry.ToolCallID)
		}
		lines := []string{lipgloss.NewStyle().Foreground(color).Bold(true).Render(terminalText(label))}
		if len(metadata) > 0 {
			lines = append(lines, muted.Render(terminalText(strings.Join(metadata, "\n"))))
		}
		if body != "" {
			body = terminalText(body)
			if entry.Role == "system" || entry.Role == "developer" {
				body = muted.Render(body)
			}
			lines = append(lines, body)
		}
		for _, call := range entry.ToolCalls {
			toolNames[call.ID] = call.Name
			lines = append(lines, toolStyle.Render(terminalText(fmt.Sprintf("Tool call: %s [%s]", call.Name, call.ID))), terminalText(sessionJSON(string(call.Arguments))))
		}
		block := lipgloss.NewStyle().
			Border(lipgloss.Border{Left: "|"}, false, false, false, true).
			BorderForeground(color).PaddingLeft(1).Width(max(1, model.view.Width-1)).
			Render(strings.Join(lines, "\n"))
		blocks = append(blocks, block)
	}
	return strings.Join(blocks, "\n\n")
}

func (model *tuiModel) renderContent() {
	var content strings.Builder
	accent := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	switch model.page {
	case "environments", "agents":
		if model.count() == 0 {
			content.WriteString("No " + model.page + ".")
		}
		for index := 0; index < model.count(); index++ {
			line := ""
			if model.page == "environments" {
				line = model.environments[index]
			} else {
				agent := model.agents[index]
				line = fmt.Sprintf("%s  |  %s  |  %d pending", agent.Name, agent.Model, agent.Pending)
				state := agent.Status
				if agent.Error != "" {
					state = "paused"
				}
				if state != "" {
					line = fmt.Sprintf("%s  |  %s  |  %d pending  |  %s", agent.Name, strings.ToUpper(state), agent.Pending, agent.Model)
				}
			}
			line = lipgloss.NewStyle().MaxWidth(max(1, model.view.Width-2)).MaxHeight(1).Render(terminalText(line))
			if index == model.cursor {
				line = accent.Render("> " + line)
			} else {
				line = "  " + line
			}
			content.WriteString(line + "\n")
		}
	case "models":
		if len(model.models) == 0 {
			content.WriteString("No configured models.")
		}
		for _, config := range model.models {
			fmt.Fprintf(&content, "%s [%s]\n%s\n%s\n\n", terminalText(config.Name), config.Provider, terminalText(config.Endpoint), terminalText(config.Description))
		}
	case "inbox":
		content.WriteString(terminalText(model.detail))
	case "session":
		model.detail = model.renderSession()
		content.WriteString(model.detail)
	}
	model.view.SetContent(lipgloss.NewStyle().Width(model.view.Width).Render(content.String()))
}

func (model tuiModel) renderToolPicker() string {
	rows := 2
	compact := model.childModelsVisible() && model.height < 26
	if compact {
		rows = 1
	}
	start := max(0, model.toolCursor-rows+1)
	var choices []string
	for index := start; index < min(len(model.toolChoices), start+rows); index++ {
		tool := model.toolChoices[index]
		mark, grant, pointer := "[ ]", "[ ]", "  "
		if model.selectedTools[tool.Name] {
			mark = "[x]"
		}
		if model.assignableTools[tool.Name] {
			grant = "[x]"
		}
		if model.toolPickerFocused() && index == model.toolCursor {
			pointer = "> "
			if model.assignPermission {
				grant = lipgloss.NewStyle().Reverse(true).Render(grant)
			} else {
				mark = lipgloss.NewStyle().Reverse(true).Render(mark)
			}
		}
		line := pointer + mark + " " + grant + "    " + terminalText(tool.Name)
		line = lipgloss.NewStyle().MaxWidth(model.width - 4).MaxHeight(1).Render(line)
		if model.toolPickerFocused() && index == model.toolCursor {
			line = lipgloss.NewStyle().Foreground(lipgloss.Color("45")).Render(line)
		}
		choices = append(choices, line)
	}
	if len(choices) == 0 {
		choices = append(choices, "No optional tools registered.")
	}
	description := ""
	if len(model.toolChoices) > 0 {
		description = terminalText(model.toolChoices[model.toolCursor].Description)
	}
	if compact {
		return "  Use Assign Tool\n" + lipgloss.NewStyle().Height(rows).Render(strings.Join(choices, "\n")) + "\nMemory + messaging: always enabled"
	}
	return "  Use Assign Tool\n" + lipgloss.NewStyle().Height(rows).Render(strings.Join(choices, "\n")) + "\n" +
		lipgloss.NewStyle().Foreground(lipgloss.Color("244")).MaxWidth(model.width-4).MaxHeight(1).Render(description) + "\n" +
		"Memory + messaging: always enabled"
}

func (model tuiModel) renderChildModelPicker() string {
	rows := 1
	if model.height >= 26 {
		rows = 2
	}
	start := max(0, model.childModelCursor-rows+1)
	choices := []string{}
	for index := start; index < min(len(model.childModelChoices), start+rows); index++ {
		name := model.childModelChoices[index]
		mark, pointer := "[ ]", "  "
		if model.allowedModels[name] {
			mark = "[x]"
		}
		if model.childModelPickerFocused() && index == model.childModelCursor {
			pointer = "> "
		}
		line := pointer + mark + " " + terminalText(name)
		choices = append(choices, lipgloss.NewStyle().MaxWidth(model.width-4).MaxHeight(1).Render(line))
	}
	if len(choices) == 0 {
		choices = append(choices, "No models configured.")
	}
	return fmt.Sprintf("Child models (%d selected)\n", len(selectedToolNames(model.allowedModels))) + lipgloss.NewStyle().Height(rows).Render(strings.Join(choices, "\n"))
}

func (model tuiModel) View() string {
	if model.width < 40 || model.height < 22 {
		return "Micro\nTerminal too small (minimum 40 x 22).\nCtrl+C to quit.\n"
	}
	title := "MICRO / " + strings.ToUpper(model.page)
	if model.page == "agents" || model.page == "inbox" || model.page == "session" {
		title += " / " + terminalText(model.environment)
	}
	if model.page == "inbox" || model.page == "session" {
		title += " / " + terminalText(model.agent)
	}
	body := model.view.View()
	help := "up/down select | enter open | n new | m models | r refresh | esc back | q quit"
	if model.page == "agents" {
		help = "enter inbox | v session | n new | e edit agent | s message | r refresh | esc back | q quit"
	}
	if model.page == "inbox" {
		help = "v session | s message | t retry | r refresh | up/down scroll | esc back | q quit"
	}
	if model.page == "session" {
		help = "s message | up/down scroll | end follow | r refresh | esc agents | q quit"
	}
	if model.page == "models" {
		help = "up/down scroll | r refresh | esc back | q quit"
	}
	if model.form != "" {
		title = "MICRO / NEW " + strings.ToUpper(model.form)
		if model.form == "agent" && model.editingAgent != "" {
			title = "MICRO / EDIT AGENT / " + terminalText(model.editingAgent)
		}
		var form strings.Builder
		for index, input := range model.inputs {
			label := model.labels[index]
			if model.form == "agent" && index == 1 {
				label += " (left/right)"
			}
			fmt.Fprintf(&form, "%s\n%s\n", label, input.View())
		}
		if model.form == "agent" || model.form == "message" {
			label := "Instructions (optional)"
			if model.form == "message" {
				label = "Message to " + terminalText(model.agent)
			}
			label = lipgloss.NewStyle().MaxWidth(model.width - 4).MaxHeight(1).Render(label)
			fmt.Fprintf(&form, "%s\n%s", label, model.text.View())
		}
		if model.form == "agent" {
			form.WriteString("\n" + model.renderToolPicker())
			if model.childModelsVisible() {
				form.WriteString("\n" + model.renderChildModelPicker())
			}
		}
		body = form.String()
		help = "tab/shift+tab field | ctrl+s submit | esc cancel | ctrl+c quit"
		if model.toolPickerFocused() || model.childModelPickerFocused() {
			help = "arrows select | space toggle | tab next | ctrl+s save | esc cancel"
		}
	}
	status := terminalText(model.status)
	if model.page == "session" && model.form == "" && status == "" {
		status = "Live | following latest"
		if !model.view.AtBottom() {
			status = "Live | reading history"
		}
	}
	color := lipgloss.Color("42")
	if model.failed {
		color = lipgloss.Color("203")
	}
	if model.busy {
		status = "Loading..."
	}
	width := model.width - 4
	return lipgloss.NewStyle().Padding(1, 2).Render(
		lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("45")).MaxWidth(width).MaxHeight(1).Render(title) + "\n\n" +
			lipgloss.NewStyle().Height(model.height-9).MaxHeight(model.height-9).Render(body) + "\n" +
			lipgloss.NewStyle().Foreground(color).MaxWidth(width).MaxHeight(1).Render(status) + "\n" +
			lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Width(width).MaxHeight(2).Render(help))
}

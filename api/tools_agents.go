package api

import (
	"context"
	"encoding/json"
	"slices"
)

func messagingTools() []Tool {
	return []Tool{
		contextTool(Tool{
			Name:        "agent_status",
			Description: "Inspect an agent's idle, queued, running, or paused status, pending count, active envelope ID, and error. Check this before sending reminders. A paused inbox requires an operator retry; more messages do not resume it.",
			Parameters:  []Parameter{{Name: "name", Type: String, Description: "Agent name to inspect", Required: true}},
		}, func(ctx context.Context, binding ToolContext, arguments struct {
			Name string `json:"name"`
		}) (string, error) {
			status, err := binding.Environment.Status(arguments.Name)
			if err != nil {
				return "", err
			}
			data, err := json.Marshal(status)
			return string(data), err
		}),
		contextTool(Tool{
			Name: "list_agents", Description: "List agent names in this environment that can receive messages.",
		}, func(ctx context.Context, binding ToolContext, arguments struct{}) (string, error) {
			data, err := json.Marshal(binding.Environment.AgentNames())
			return string(data), err
		}),
		contextTool(Tool{
			Name:        "message",
			Description: "Queue a message to another existing agent and return its accepted ID plus a recipient status snapshot, not an answer or task completion. A paused recipient cannot process it until an operator retries its inbox. Use agent_status before sending reminders. Replies and runtime failure notices arrive as separate inbox turns. When Source is \"agent\", send substantive replies to the incoming envelope's Sender and set reply_to to its ID. When Source is not \"agent\", reply with normal assistant text; client labels such as \"tui\" or \"user\" and the runtime are not agent recipients. You may still delegate work to existing agents. Do not send acknowledgement-only replies or wait for another agent in this turn.",
			Parameters: []Parameter{
				{Name: "to", Type: String, Description: "Recipient agent name", Required: true},
				{Name: "content", Type: String, Description: "Message content", Required: true},
				{Name: "reply_to", Type: String, Description: "Envelope ID being answered, or empty for a new message"},
			},
		}, func(ctx context.Context, binding ToolContext, arguments struct {
			To      string `json:"to"`
			Content string `json:"content"`
			ReplyTo string `json:"reply_to"`
		}) (string, error) {
			incoming, _ := ctx.Value(envelopeContextKey{}).(Envelope)
			receipt, err := binding.Environment.SendAgentMessage(ctx, binding.Caller, Envelope{To: arguments.To, Content: arguments.Content, ReplyTo: arguments.ReplyTo, ConversationID: incoming.ConversationID})
			if err != nil {
				return "", err
			}
			data, err := json.Marshal(receipt)
			return string(data), err
		}),
	}
}

func CreateAgentTool() Tool {
	tool := Tool{
		Name:        "create_agent",
		Description: "Create an agent in your environment using a configured model name. Optionally grant tools from your Assign permissions, independently of tools you can use. Children receive memory and messaging tools automatically, but no Assign permissions. Use message afterwards to delegate work and request a reply.",
		Parameters: []Parameter{
			{Name: "name", Type: String, Description: "Unique agent name using letters, digits, underscores, or hyphens", Required: true},
			{Name: "model", Type: String, Description: "Exact configured model name for the new agent. Choose from the listed values; do not invent model names.", Required: true},
			{Name: "instructions", Type: String, Description: "Role and working instructions for the new agent"},
			{Name: "tools", Type: Array, Items: String, Description: "Optional tool names to grant for use. Must be in your Assign permissions; omitted or empty grants no optional tools."},
		},
	}
	tool.bind = func(binding ToolContext) Tool {
		bound := tool
		bound.Parameters = slices.Clone(tool.Parameters)
		if models, err := ListModels(); err == nil {
			for _, model := range models {
				if slices.Contains(binding.AllowedModels, model.Name) {
					bound.Parameters[1].Enum = append(bound.Parameters[1].Enum, model.Name)
				}
			}
		}
		if len(bound.Parameters[1].Enum) == 0 {
			bound.Description += " No child models are allowed; do not call this tool until model permissions are configured."
		}
		bound.Description += " Children granted create_agent inherit your child-model allowlist, but no Assign tool permissions."
		catalogue := make([]ToolSummary, 0, len(binding.AssignableTools))
		for _, allowed := range binding.AssignableTools {
			catalogue = append(catalogue, ToolSummary{Name: allowed.Name, Description: allowed.Description})
		}
		encoded, _ := json.Marshal(catalogue)
		bound.Description += " Tools you may assign: " + string(encoded)
		bound.Execute = JSONHandler(func(ctx context.Context, arguments ChildAgentRequest) (string, error) {
			if _, err := binding.Environment.CreateChildAgent(ctx, binding.Caller, arguments); err != nil {
				return "", err
			}
			result, err := json.Marshal(map[string]string{"name": arguments.Name, "status": "created"})
			return string(result), err
		})
		return bound
	}
	return tool
}

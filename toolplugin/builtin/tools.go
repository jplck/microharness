package builtin

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jplck/micro/toolplugin"
)

func Tools() []toolplugin.Tool {
	text := func(name, description string, required bool) toolplugin.Parameter {
		return toolplugin.Parameter{Name: name, Type: toolplugin.String, Description: description, Required: required}
	}
	definitions := []toolplugin.Definition{
		{Name: "search_memory", Automatic: true, Description: "Search shared memory", Parameters: []toolplugin.Parameter{text("query", "Search query", true)}},
		{Name: "write_memory", Automatic: true, Description: "Write information to shared memory", Parameters: []toolplugin.Parameter{text("kind", "fact, episode, or procedure", true), text("content", "Information to remember", true)}},
		{Name: "update_memory", Automatic: true, Description: "Update an existing memory", Parameters: []toolplugin.Parameter{text("id", "Memory ID", true), text("kind", "fact, episode, or procedure", true), text("content", "Updated information", true)}},
		{Name: "agent_status", Automatic: true, Description: "Inspect idle, queued, running, or paused status, pending count, active envelope ID, and error. Check before reminders. A paused inbox requires an operator retry; more messages do not resume it.", Parameters: []toolplugin.Parameter{text("name", "Agent name", true)}},
		{Name: "list_agents", Automatic: true, Description: "List agent names in this environment that can receive messages."},
		{Name: "message", Automatic: true, Description: "Queue a message to another existing agent. Returns acceptance and recipient status, not task completion. Replies arrive in later turns. When Source is \"agent\", send substantive replies to Sender and set reply_to to its ID. When Source is not \"agent\", reply with normal assistant text; client labels such as \"tui\" or \"user\" and the runtime are not agent recipients. Do not wait, send acknowledgement-only replies, or resend blindly to paused agents; use agent_status.", Parameters: []toolplugin.Parameter{text("to", "Recipient agent", true), text("content", "Message content", true), text("reply_to", "Envelope ID being answered, or empty", false)}},
		{Name: "create_agent", Description: "Create an agent using a permitted model and tools from your Assign permissions. Children receive automatic tools but no Assign permissions. Use message afterwards to delegate work.", Parameters: []toolplugin.Parameter{text("name", "Unique agent name", true), text("model", "Exact configured and permitted model name", true), text("instructions", "Role and instructions", false), {Name: "tools", Type: toolplugin.Array, Items: toolplugin.String, Description: "Optional tools from your Assign permissions"}}},
		{Name: "get_time", Description: "Get the current time in a specific location", Parameters: []toolplugin.Parameter{text("location", "Location", true)}},
		{Name: "create_tool", Description: "Build, test, and register a private Go binary plugin for immediate use. Not sandboxed. Supply a complete package main using only the standard library and github.com/jplck/micro/toolplugin. main calls toolplugin.Serve([]toolplugin.Tool{...}) with exactly one tool. Tool embeds Definition{Name, Description, Parameters: []toolplugin.Parameter{{Name, Type: toolplugin.String, Description, Required: true}}}, and Call: toolplugin.Handler(func(args struct{Input string})(string,error){return args.Input,nil}). Supported parameter types: String, Integer, Number, Boolean, Array (set Items to a primitive type); Enum is []string. Handle Serve errors by printing to stderr and exiting nonzero. The SDK implements the versioned describe/call JSON contract. No external dependencies or host operations. test_arguments is a JSON object encoded as a string; result must match expected_output exactly. Names cannot overwrite tools. Tools persist privately and cannot be assigned to children.", Parameters: []toolplugin.Parameter{text("name", "Expected tool name, [a-z][a-z0-9_]{0,63}", true), text("source", "Complete Go main package, max 64 KiB", true), text("test_arguments", "JSON object encoded as a string", true), text("expected_output", "Exact expected result string", true)}},
	}
	tools := make([]toolplugin.Tool, 0, len(definitions))
	for _, definition := range definitions {
		name := definition.Name
		tools = append(tools, toolplugin.Tool{Definition: definition, Call: func(arguments json.RawMessage) (string, error) {
			if name == "get_time" {
				var request struct{ Location string }
				if err := json.Unmarshal(arguments, &request); err != nil {
					return "", err
				}
				return fmt.Sprintf("Current time in %s: %s", request.Location, time.Now().Format(time.RFC3339)), nil
			}
			var result json.RawMessage
			if err := toolplugin.Host(name, arguments, &result); err != nil {
				return "", err
			}
			if name == "search_memory" || name == "write_memory" || name == "update_memory" {
				var text string
				if err := json.Unmarshal(result, &text); err != nil {
					return "", err
				}
				return text, nil
			}
			return string(result), nil
		}})
	}
	return tools
}

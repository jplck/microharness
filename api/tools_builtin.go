package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

func DefaultTools() ToolRegistry {
	text := func(name, description string, required bool) Parameter {
		return Parameter{Name: name, Type: String, Description: description, Required: required}
	}
	definitions := []Tool{
		{Name: "search_memory", Automatic: true, Description: "Search shared memory", Parameters: []Parameter{text("query", "Search query", true)}},
		{Name: "write_memory", Automatic: true, Description: "Write information to shared memory", Parameters: []Parameter{text("kind", "fact, episode, or procedure", true), text("content", "Information to remember", true)}},
		{Name: "update_memory", Automatic: true, Description: "Update an existing memory", Parameters: []Parameter{text("id", "Memory ID", true), text("kind", "fact, episode, or procedure", true), text("content", "Updated information", true)}},
		{Name: "agent_status", Automatic: true, Description: "Inspect idle, queued, running, or paused status, pending count, active envelope ID, and error. Check before reminders. A paused inbox requires an operator retry; more messages do not resume it.", Parameters: []Parameter{text("name", "Agent name", true)}},
		{Name: "message_status", Automatic: true, Description: "Inspect an outgoing message by its receipt ID. States: queued, delivered, completed, retracted, cancelled. Delivered means committed to the recipient's run, not proof that its task succeeded. Only your own outgoing messages are accessible.", Parameters: []Parameter{text("id", "Outgoing envelope ID from a message receipt", true)}},
		{Name: "retract_message", Automatic: true, Description: "Retract your own outgoing message only while it is queued and not being delivered. Use its receipt ID to withdraw accidental duplicates. Retraction is recorded permanently and repeating it is harmless. Delivered messages cannot be undone; send a steering message targeting the original run instead. Retracting a queued run also cancels its queued steering.", Parameters: []Parameter{text("id", "Outgoing envelope ID to retract", true)}},
		{Name: "list_agents", Automatic: true, Description: "List agent names in this environment that can receive messages."},
		{Name: "message", Automatic: true, Description: "Queue a message to another existing agent. Returns acceptance, envelope ID, delivery_status, and recipient status, not task completion. Keep the ID; retract_message can withdraw an undelivered duplicate. Set steer to the original request's envelope ID to influence that unfinished run instead of queueing an independent task. Steering is delivered at safe checkpoints, not inside an in-flight model or tool call; unstarted tool calls are cancelled so the model can reconsider. A finished target is rejected. Replies arrive in later turns. When Source is \"agent\", send substantive replies to Sender and set reply_to to its ID. When Source is not \"agent\", reply with normal assistant text; client labels such as \"tui\" or \"user\" and the runtime are not agent recipients. Do not wait, send acknowledgement-only replies, or resend blindly to paused agents; use agent_status.", Parameters: []Parameter{text("to", "Recipient agent", true), text("content", "Message content", true), text("reply_to", "Envelope ID being answered, or empty", false), text("steer", "Original envelope ID of an unfinished run to steer; omit for an independent message", false)}},
		{Name: "create_agent", Description: "Create an agent using a permitted model and tools from your Assign permissions. Children receive automatic tools but no Assign permissions. Use message afterwards to delegate work.", Parameters: []Parameter{text("name", "Unique agent name", true), text("model", "Exact configured and permitted model name", true), text("instructions", "Role and instructions", false), {Name: "tools", Type: Array, Items: String, Description: "Optional tools from your Assign permissions"}}},
		{Name: "get_time", Description: "Get the current time in a specific location", Parameters: []Parameter{text("location", "Location", true)}},
		{Name: "create_tool", Description: "Build, test, and register a private Go binary plugin for immediate use. Not sandboxed. Supply a complete package main using only the standard library and github.com/jplck/micro/toolplugin. main calls toolplugin.Serve([]toolplugin.Tool{...}) with exactly one tool. Tool embeds Definition{Name, Description, Parameters: []toolplugin.Parameter{{Name, Type: toolplugin.String, Description, Required: true}}}, and Call: toolplugin.Handler(func(args struct{Input string})(string,error){return args.Input,nil}). Supported parameter types: String, Integer, Number, Boolean, Array (set Items to a primitive type); Enum is []string. Handle Serve errors by printing to stderr and exiting nonzero. The SDK implements the versioned describe/call JSON contract. No external dependencies or host operations. Required test_source is a Go package main test file with a passing, non-skipped TestToolSuccess that exercises the actual handler's successful behavior and asserts results, not only invalid-input errors. Use deterministic fixtures such as httptest for changing web data, or explicit timestamps for time conversions. Factor the handler into a testable function. Production failures must return errors, not success-shaped ERROR strings. test_arguments is a JSON object encoded as a string; a successful sample call must match expected_output exactly. Names cannot overwrite tools; use update_tool to fix your own existing private tool without creating versioned names. Tools persist privately and cannot be assigned to children.", Parameters: []Parameter{text("name", "Expected tool name, [a-z][a-z0-9_]{0,63}", true), text("source", "Complete Go main package, max 64 KiB", true), text("test_source", "Go package main tests including TestToolSuccess with deterministic successful-handler assertions", true), text("test_arguments", "JSON object encoded as a string", true), text("expected_output", "Exact expected successful result string", true)}},
		{Name: "update_tool", Automatic: true, Description: "Build and validate a replacement for an existing private tool you own, keeping its name. Requires create_tool Use permission. Supply the same source, test_source (passing non-skipped TestToolSuccess), test_arguments, and expected_output contract as create_tool. Validate the successful handler path with deterministic fixtures, not only errors. Registration changes atomically only after all validation succeeds; failures preserve the current tool, and already-started calls retain their version. Cannot update native tools or another agent's tools.", Parameters: []Parameter{text("name", "Existing private tool name", true), text("source", "Complete replacement Go main package, max 64 KiB", true), text("test_source", "Go package main tests including TestToolSuccess with deterministic successful-handler assertions", true), text("test_arguments", "JSON object encoded as a string", true), text("expected_output", "Exact expected successful result string", true)}},
	}
	registry := ToolRegistry{}
	for _, tool := range definitions {
		tool.bind = func(binding ToolContext) Tool {
			bound := tool
			if tool.Name == "create_agent" {
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
			}
			bound.Execute = func(ctx context.Context, arguments json.RawMessage) (string, error) {
				timeout := 30 * time.Second
				if tool.Name == "create_tool" || tool.Name == "update_tool" {
					timeout = 120 * time.Second
				}
				ctx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				if err := ctx.Err(); err != nil {
					return "", err
				}
				if binding.Environment == nil || binding.Caller == "" {
					return "", fmt.Errorf("built-in tool requires caller context")
				}
				if len(arguments) > 128<<10 {
					return "", fmt.Errorf("tool arguments exceed 128 KiB")
				}
				arguments = bytes.TrimSpace(arguments)
				if !json.Valid(arguments) || arguments[0] != '{' {
					return "", fmt.Errorf("invalid tool arguments: expected a JSON object")
				}
				result, err := executeBuiltin(ctx, binding, tool.Name, arguments)
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				if err != nil {
					return "", err
				}
				output, ok := result.(string)
				if !ok {
					data, err := json.Marshal(result)
					if err != nil {
						return "", err
					}
					output = string(data)
				}
				if len(output) > 1<<20 {
					return "", fmt.Errorf("tool result exceeds 1 MiB")
				}
				return output, nil
			}
			return bound
		}
		registry[tool.Name] = tool
	}
	return registry
}

func executeBuiltin(ctx context.Context, binding ToolContext, name string, raw json.RawMessage) (any, error) {
	env := binding.Environment
	switch name {
	case "search_memory", "write_memory", "update_memory":
		var arguments struct{ Query, ID, Kind, Content string }
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return nil, err
		}
		switch name {
		case "search_memory":
			return fmt.Sprintf("Search results for query '%s': %v", arguments.Query, env.MemoryStore.Search(arguments.Query)), nil
		case "write_memory":
			err := env.MemoryStore.Add(ctx, Memory{Kind: arguments.Kind, Content: arguments.Content})
			return fmt.Sprintf("Written to memory: '%s'", arguments.Content), err
		default:
			_, err := env.MemoryStore.Update(ctx, Memory{ID: arguments.ID, Kind: arguments.Kind, Content: arguments.Content})
			return fmt.Sprintf("Updated memory: '%s'", arguments.Content), err
		}
	case "list_agents":
		return env.AgentNames(), nil
	case "agent_status":
		var arguments struct{ Name string }
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return nil, err
		}
		return env.Status(arguments.Name)
	case "message_status", "retract_message":
		var arguments struct{ ID string }
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return nil, err
		}
		if name == "message_status" {
			return env.SentDelivery(binding.Caller, arguments.ID)
		}
		return env.RetractMessage(ctx, "agent", binding.Caller, arguments.ID)
	case "message":
		var arguments struct {
			To, Content string
			ReplyTo     string `json:"reply_to"`
			Steer       string `json:"steer"`
		}
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return nil, err
		}
		incoming, _ := ctx.Value(envelopeContextKey{}).(Envelope)
		conversation := incoming.ConversationID
		if arguments.Steer != "" {
			conversation = ""
		}
		return env.SendAgentMessage(ctx, binding.Caller, Envelope{To: arguments.To, Content: arguments.Content, ReplyTo: arguments.ReplyTo, ConversationID: conversation, Steer: arguments.Steer})
	case "create_agent":
		var arguments ChildAgentRequest
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return nil, err
		}
		_, err := env.CreateChildAgent(ctx, binding.Caller, arguments)
		return map[string]string{"name": arguments.Name, "status": "created"}, err
	case "create_tool", "update_tool":
		var arguments CreatePluginRequest
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return nil, err
		}
		if name == "update_tool" {
			err := env.UpdatePlugin(ctx, binding.Caller, arguments)
			return map[string]string{"name": arguments.Name, "status": "updated"}, err
		}
		err := env.CreatePlugin(ctx, binding.Caller, arguments)
		return map[string]string{"name": arguments.Name, "status": "created"}, err
	case "get_time":
		var arguments struct{ Location string }
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return nil, err
		}
		return fmt.Sprintf("Current time in %s: %s", arguments.Location, time.Now().Format(time.RFC3339)), nil
	}
	return nil, fmt.Errorf("unknown built-in tool %q", name)
}

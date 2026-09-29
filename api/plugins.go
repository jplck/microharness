package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/jplck/micro/toolplugin"
)

type pluginBinary struct {
	Path        string
	Builtin     bool
	Definitions []toolplugin.Definition
}

func builtinPluginPath() string {
	if path := os.Getenv("MICRO_BUILTIN_PLUGIN"); path != "" {
		absolute, _ := filepath.Abs(path)
		return absolute
	}
	path, _ := filepath.Abs("plugins/bin/builtin")
	return path
}

func binaryTool(binary *pluginBinary, definition toolplugin.Definition) Tool {
	tool := Tool{plugin: binary, Name: definition.Name, Description: definition.Description, Parameters: definition.Parameters, Automatic: definition.Automatic}
	tool.bind = func(binding ToolContext) Tool {
		bound := tool
		if binary.Builtin && tool.Name == "create_agent" {
			bound.Parameters = slices.Clone(tool.Parameters)
			modelIndex := slices.IndexFunc(bound.Parameters, func(parameter Parameter) bool { return parameter.Name == "model" })
			if modelIndex >= 0 {
				bound.Parameters[modelIndex].Enum = nil
			}
			if models, err := ListModels(); err == nil {
				for _, model := range models {
					if modelIndex >= 0 && slices.Contains(binding.AllowedModels, model.Name) {
						bound.Parameters[modelIndex].Enum = append(bound.Parameters[modelIndex].Enum, model.Name)
					}
				}
			}
			if modelIndex < 0 || len(bound.Parameters[modelIndex].Enum) == 0 {
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
			return binary.call(ctx, definition.Name, arguments, binding)
		}
		return bound
	}
	return tool
}

type boundedPluginOutput struct {
	buffer   bytes.Buffer
	cancel   context.CancelFunc
	exceeded bool
}

func (output *boundedPluginOutput) Write(data []byte) (int, error) {
	if len(data) > (1<<20)-output.buffer.Len() {
		output.exceeded = true
		output.cancel()
		return 0, fmt.Errorf("plugin output exceeds 1 MiB")
	}
	return output.buffer.Write(data)
}

func runPluginCommand(ctx context.Context, timeout time.Duration, path string, args []string, directory string, environment []string, input []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(ctx, path, args...)
	command.Dir, command.Env, command.Stdin = directory, environment, bytes.NewReader(input)
	command.WaitDelay = time.Second
	stdout, stderr := &boundedPluginOutput{cancel: cancel}, &boundedPluginOutput{cancel: cancel}
	command.Stdout, command.Stderr = stdout, stderr
	err := command.Run()
	if stdout.exceeded || stderr.exceeded {
		return nil, fmt.Errorf("plugin output exceeds 1 MiB")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("plugin process: %w: %s", err, stderr.buffer.String())
	}
	return stdout.buffer.Bytes(), nil
}

func (binary *pluginBinary) call(ctx context.Context, name string, arguments json.RawMessage, binding ToolContext) (string, error) {
	timeout := 30 * time.Second
	if binary.Builtin && name == "create_tool" {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := json.Marshal(toolplugin.Request{ProtocolVersion: 1, Arguments: arguments})
	if err != nil {
		return "", err
	}
	if len(request) > 128<<10 {
		return "", fmt.Errorf("plugin request exceeds 128 KiB")
	}
	directory, err := os.MkdirTemp("", "micro-plugin-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(directory)
	environment := []string{"PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"}
	if binary.Builtin && name != "get_time" {
		if binding.Environment == nil || binding.Caller == "" {
			return "", fmt.Errorf("plugin requires caller context")
		}
		socket := filepath.Join(directory, "host.sock")
		listener, err := net.Listen("unix", socket)
		if err != nil {
			return "", err
		}
		defer listener.Close()
		server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			if request.Method != http.MethodPost || request.URL.Path != "/"+name {
				http.Error(w, "operation not permitted", http.StatusForbidden)
				return
			}
			data, err := io.ReadAll(http.MaxBytesReader(w, request.Body, 128<<10))
			var result any
			if err == nil {
				result, err = pluginHostOperation(ctx, binding, name, data)
			}
			encoded, encodeErr := json.Marshal(result)
			if err == nil {
				err = encodeErr
			}
			response := toolplugin.Reply(string(encoded), err)
			response.Code = pluginErrorCode(err)
			writeJSONResponse(w, http.StatusOK, response)
		})}
		done := make(chan struct{})
		go func() { defer close(done); _ = server.Serve(listener) }()
		defer func() { _ = server.Close(); <-done }()
		environment = append(environment, "MICRO_PLUGIN_SOCKET="+socket)
	}
	output, err := runPluginCommand(ctx, timeout, binary.Path, []string{"call", name}, directory, environment, request)
	if err != nil {
		return "", err
	}
	var response toolplugin.Response
	if err := json.Unmarshal(output, &response); err != nil {
		return "", fmt.Errorf("invalid plugin response: %w", err)
	}
	if response.ProtocolVersion != 1 {
		return "", fmt.Errorf("unsupported plugin protocol %d", response.ProtocolVersion)
	}
	if response.Error != "" {
		if cause := pluginErrors[response.Code]; cause != nil {
			return "", fmt.Errorf("%w: %s", cause, response.Error)
		}
		return "", errors.New(response.Error)
	}
	return response.Result, nil
}

var pluginErrors = map[string]error{"invalid_tool": ErrInvalidTool, "model_not_allowed": ErrModelNotAllowed, "model_not_found": ErrModelNotFound, "agent_not_found": ErrAgentNotFound, "agent_exists": ErrAgentExists, "invalid_name": ErrInvalidName, "invalid_envelope": ErrInvalidEnvelope, "closed": ErrEnvironmentClosed}

func pluginErrorCode(err error) string {
	for code, candidate := range pluginErrors {
		if errors.Is(err, candidate) {
			return code
		}
	}
	return ""
}

func pluginHostOperation(ctx context.Context, binding ToolContext, operation string, raw json.RawMessage) (any, error) {
	env := binding.Environment
	switch operation {
	case "search_memory", "write_memory", "update_memory":
		var arguments struct{ Query, ID, Kind, Content string }
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return nil, err
		}
		switch operation {
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
	case "message":
		var arguments struct {
			To, Content string
			ReplyTo     string `json:"reply_to"`
		}
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return nil, err
		}
		incoming, _ := ctx.Value(envelopeContextKey{}).(Envelope)
		return env.SendAgentMessage(ctx, binding.Caller, Envelope{To: arguments.To, Content: arguments.Content, ReplyTo: arguments.ReplyTo, ConversationID: incoming.ConversationID})
	case "create_agent":
		var arguments ChildAgentRequest
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return nil, err
		}
		_, err := env.CreateChildAgent(ctx, binding.Caller, arguments)
		return map[string]string{"name": arguments.Name, "status": "created"}, err
	}
	if operation == "create_tool" {
		var arguments CreatePluginRequest
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return nil, err
		}
		err := env.CreatePlugin(ctx, binding.Caller, arguments)
		return map[string]string{"name": arguments.Name, "status": "created"}, err
	}
	return nil, fmt.Errorf("unsupported host operation %q", operation)
}

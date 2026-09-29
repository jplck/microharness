package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/jplck/micro/toolplugin"
)

type pluginBinary struct {
	Path        string
	Definitions []toolplugin.Definition
}

func binaryTool(binary *pluginBinary, definition toolplugin.Definition) Tool {
	return Tool{
		plugin: binary, Name: definition.Name, Description: definition.Description,
		Parameters: definition.Parameters, Automatic: definition.Automatic,
		Execute: func(ctx context.Context, arguments json.RawMessage) (string, error) {
			return binary.call(ctx, definition.Name, arguments)
		},
	}
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

func (binary *pluginBinary) call(ctx context.Context, name string, arguments json.RawMessage) (string, error) {
	timeout := 30 * time.Second
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

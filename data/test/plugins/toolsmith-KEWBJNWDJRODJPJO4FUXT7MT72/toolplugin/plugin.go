package toolplugin

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

type ParameterType string

const (
	String  ParameterType = "string"
	Integer ParameterType = "integer"
	Number  ParameterType = "number"
	Boolean ParameterType = "boolean"
	Array   ParameterType = "array"
)

type Parameter struct {
	Name        string        `json:"name"`
	Type        ParameterType `json:"type"`
	Description string        `json:"description"`
	Required    bool          `json:"required"`
	Items       ParameterType `json:"items,omitempty"`
	Enum        []string      `json:"enum,omitempty"`
}

type Definition struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  []Parameter `json:"parameters,omitempty"`
	Automatic   bool        `json:"automatic,omitempty"`
}

type Tool struct {
	Definition
	Call func(json.RawMessage) (string, error)
}

const ProtocolVersion = 1

//go:embed plugin.go
var Source string

type Manifest struct {
	ProtocolVersion int          `json:"protocol_version"`
	Tools           []Definition `json:"tools"`
}

type Request struct {
	ProtocolVersion int             `json:"protocol_version"`
	Arguments       json.RawMessage `json:"arguments"`
}

type Response struct {
	ProtocolVersion int    `json:"protocol_version"`
	Result          string `json:"result"`
	Error           string `json:"error,omitempty"`
	Code            string `json:"code,omitempty"`
}

type RemoteError struct {
	Message string
	Code    string
}

func (err *RemoteError) Error() string { return err.Message }

func Reply(result string, err error) Response {
	response := Response{ProtocolVersion: ProtocolVersion, Result: result}
	if err != nil {
		response.Result, response.Error = "", err.Error()
		var remote *RemoteError
		if errors.As(err, &remote) {
			response.Code = remote.Code
		}
	}
	return response
}

func Serve(tools []Tool) error { return Run(os.Args[1:], os.Stdin, os.Stdout, tools) }

func Run(args []string, input io.Reader, output io.Writer, tools []Tool) error {
	if len(args) == 1 && args[0] == "describe" {
		definitions := make([]Definition, 0, len(tools))
		for _, tool := range tools {
			definitions = append(definitions, tool.Definition)
		}
		return json.NewEncoder(output).Encode(Manifest{ProtocolVersion: ProtocolVersion, Tools: definitions})
	}
	if len(args) != 2 || args[0] != "call" {
		return fmt.Errorf("usage: plugin describe | plugin call <tool>")
	}
	data, err := io.ReadAll(io.LimitReader(input, (128<<10)+1))
	if err != nil {
		return err
	}
	var request Request
	if len(data) > 128<<10 || json.Unmarshal(data, &request) != nil || request.ProtocolVersion != ProtocolVersion || len(request.Arguments) == 0 || request.Arguments[0] != '{' {
		return json.NewEncoder(output).Encode(Reply("", fmt.Errorf("expected protocol_version 1 and an arguments object, max 128 KiB")))
	}
	for _, tool := range tools {
		if tool.Name != args[1] {
			continue
		}
		result, err := tool.Call(request.Arguments)
		return json.NewEncoder(output).Encode(Reply(result, err))
	}
	return json.NewEncoder(output).Encode(Reply("", fmt.Errorf("unknown tool %q", args[1])))
}

func Handler[Arguments any](call func(Arguments) (string, error)) func(json.RawMessage) (string, error) {
	return func(raw json.RawMessage) (string, error) {
		var arguments Arguments
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		return call(arguments)
	}
}

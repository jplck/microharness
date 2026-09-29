package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jplck/micro/toolplugin"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

type ParameterType = toolplugin.ParameterType

const (
	String  ParameterType = "string"
	Integer ParameterType = "integer"
	Number  ParameterType = "number"
	Boolean ParameterType = "boolean"
	Array   ParameterType = "array"
)

type Parameter = toolplugin.Parameter

type ToolContext struct {
	Environment     *AgentEnvironment
	Caller          string
	AssignableTools []Tool
	AllowedModels   []string
}

type Tool struct {
	plugin      *pluginBinary
	bind        func(ToolContext) Tool
	Automatic   bool
	Name        string
	Description string
	Parameters  []Parameter
	Execute     func(context.Context, json.RawMessage) (string, error)
}

type ToolSummary struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (t Tool) AsOpenAITool() openai.ChatCompletionToolUnionParam {
	properties := make(map[string]any, len(t.Parameters))
	required := []string{}

	for _, p := range t.Parameters {
		property := map[string]any{
			"type":        string(p.Type),
			"description": p.Description,
		}
		if p.Type == Array {
			property["items"] = map[string]any{"type": string(p.Items)}
		}
		if len(p.Enum) > 0 {
			property["enum"] = p.Enum
		}
		properties[p.Name] = property
		if p.Required {
			required = append(required, p.Name)
		}
	}

	return openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
		Name:        t.Name,
		Description: openai.String(t.Description),
		Parameters: shared.FunctionParameters{
			"type":                 "object",
			"properties":           properties,
			"required":             required,
			"additionalProperties": false,
		},
	})
}

func JSONHandler[T any](
	handler func(context.Context, T) (string, error),
) func(context.Context, json.RawMessage) (string, error) {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var args T
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("invalid tool arguments: %w", err)
		}
		return handler(ctx, args)
	}
}

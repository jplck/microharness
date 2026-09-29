package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
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

type Tool struct {
	bindEnvironment func(*AgentEnvironment, string, []Tool, []string) Tool
	Name            string
	Description     string
	Parameters      []Parameter
	Execute         func(context.Context, json.RawMessage) (string, error)
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

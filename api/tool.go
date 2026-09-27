package api

import (
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

type ParameterType string

const (
    String  ParameterType = "string"
    Integer ParameterType = "integer"
    Number  ParameterType = "number"
    Boolean ParameterType = "boolean"
)

type Parameter struct {
    Name        string
    Type        ParameterType
    Description string
    Required    bool
}

type Tool struct {
    Name        string
    Description string
    Parameters  []Parameter
}

func (t Tool) AsOpenAITool() openai.ChatCompletionToolUnionParam {
    properties := make(map[string]any, len(t.Parameters))
    required := []string{}

    for _, p := range t.Parameters {
        properties[p.Name] = map[string]any{
            "type":        string(p.Type),
            "description": p.Description,
        }
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

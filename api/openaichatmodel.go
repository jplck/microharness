package api

import (
	"context"
	"fmt"
	"time"

	"github.com/openai/openai-go/v3"
)

type OpenAIChatModel struct {
	Config Model
	Client openai.Client
}

func (model *OpenAIChatModel) Call(ctx context.Context, prompt string, tools []Tool) (string, error) {
	if model.Config.Endpoint == "" {
		return "", fmt.Errorf("endpoint is required")
	}
	if model.Config.APIKeyEnv != "" && model.Config.APIKey == "" {
		return "", fmt.Errorf("API key is missing for model %q", model.Config.Name)
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	var openaiTools []openai.ChatCompletionToolUnionParam
    for _, tool := range tools {
        openaiTools = append(openaiTools, tool.AsOpenAITool())
    }

	result, err := model.Client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: model.Config.Name,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(prompt),
		},
		Tools: openaiTools,
	})
	if err != nil {
		return "", fmt.Errorf("call model %q: %w", model.Config.Name, err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("model %q returned no choices", model.Config.Name)
	}
	return result.Choices[0].Message.Content, nil
}

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/openai/openai-go/v3"
)

type OpenAIChatModel struct {
	Config Model
	Client openai.Client
}

type Message struct {
	Role       string
	Content    string     `json:"Content,omitempty"`
	ToolCalls  []ToolCall `json:"ToolCalls,omitempty"`
	ToolCallID string     `json:"ToolCallID,omitempty"`
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

func (model *OpenAIChatModel) Call(ctx context.Context, messages []Message, tools []Tool) (Message, error) {
	if model.Config.Endpoint == "" {
		return Message{}, fmt.Errorf("endpoint is required")
	}
	if model.Config.APIKeyEnv != "" && model.Config.APIKey == "" {
		return Message{}, fmt.Errorf("API key is missing for model %q", model.Config.Name)
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	var openaiTools []openai.ChatCompletionToolUnionParam
	for _, tool := range tools {
		openaiTools = append(openaiTools, tool.AsOpenAITool())
	}

	chatMessages := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages))

	for _, message := range messages {
		switch message.Role {
		case "system":
			chatMessages = append(chatMessages, openai.SystemMessage(message.Content))
		case "developer":
			chatMessages = append(chatMessages, openai.DeveloperMessage(message.Content))
		case "user":
			chatMessages = append(chatMessages, openai.UserMessage(message.Content))
		case "assistant":
			assistant := openai.AssistantMessage(message.Content)
			for _, toolCall := range message.ToolCalls {
				assistant.OfAssistant.ToolCalls = append(
					assistant.OfAssistant.ToolCalls,
					openai.ChatCompletionMessageToolCallUnionParam{
						OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
							ID: toolCall.ID,
							Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
								Name:      toolCall.Name,
								Arguments: string(toolCall.Arguments),
							},
						},
					},
				)
			}
			chatMessages = append(chatMessages, assistant)
		case "tool":
			if message.ToolCallID == "" {
				return Message{}, fmt.Errorf("tool message requires a tool call ID")
			}
			chatMessages = append(chatMessages,
				openai.ToolMessage(message.Content, message.ToolCallID),
			)
		default:
			return Message{}, fmt.Errorf("unsupported message role %q", message.Role)
		}
	}

	result, err := model.Client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    model.Config.Name,
		Messages: chatMessages,
		Tools:    openaiTools,
	})
	if err != nil {
		return Message{}, fmt.Errorf("call model %q: %w", model.Config.Name, err)
	}
	if len(result.Choices) == 0 {
		return Message{}, fmt.Errorf("model %q returned no choices", model.Config.Name)
	}
	message := result.Choices[0].Message
	response := Message{
		Role:    "assistant",
		Content: message.Content,
	}

	for _, toolCall := range message.ToolCalls {
		if toolCall.Type != "function" {
			return Message{}, fmt.Errorf("unsupported tool call type %q", toolCall.Type)
		}
		response.ToolCalls = append(response.ToolCalls, ToolCall{
			ID:        toolCall.ID,
			Name:      toolCall.Function.Name,
			Arguments: json.RawMessage(toolCall.Function.Arguments),
		})
	}

	return response, nil
}

package api

import (
	"context"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

type OpenAIModel struct {
	Config Model
}

func (model *OpenAIModel) Call(ctx context.Context, messages []Message, tools []Tool) (Message, error) {
	client := &OpenAIChatModel{
		Config: model.Config,
		Client: openai.NewClient(
			option.WithBaseURL(model.Config.Endpoint),
			option.WithAPIKey(model.Config.APIKey),
			option.WithMaxRetries(0),
		),
	}
	return client.Call(ctx, messages, tools)
}

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

type Provider string

const (
	ProviderOpenAI Provider = "openai"
	ProviderAzure  Provider = "azure"
)

func (provider *Provider) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}

	switch Provider(value) {
	case ProviderOpenAI, ProviderAzure:
		*provider = Provider(value)
		return nil
	default:
		return fmt.Errorf("unsupported provider %q", value)
	}
}

type Model struct {
	Name        string   `json:"name"`
	Endpoint    string   `json:"endpoint"`
	Description string   `json:"description"`
	Provider    Provider `json:"provider"`
	APIKeyEnv   string   `json:"apiKeyEnv,omitempty"`
	APIKey      string   `json:"-"`
}

type Authentication struct {
	APIKey          string
	TokenCredential azcore.TokenCredential
}

func ListModels() ([]Model, error) {
	data, err := os.ReadFile("models.json")
	if err != nil {
		return nil, fmt.Errorf("read models: %w", err)
	}

	var result struct {
		Models []Model `json:"models"`
	}

	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}

	for index := range result.Models {
		model := &result.Models[index]

		if model.APIKeyEnv == "" {
			continue
		}

		apiKey, ok := os.LookupEnv(model.APIKeyEnv)
		if ok {
			model.APIKey = apiKey
		}
	}

	return result.Models, nil
}

type ModelCall interface {
	Call(ctx context.Context, messages []Message, tools []Tool) (Message, error)
}

func NewModelClient(config Model) (ModelCall, error) {
	switch config.Provider {
	case ProviderOpenAI:
		return &OpenAIModel{Config: config}, nil
	case ProviderAzure:
		return &AzureModel{Config: config}, nil
	default:
		return nil, fmt.Errorf(
			"unsupported provider %q", config.Provider,
		)
	}
}

func GetModelByName(name string) (*Model, error) {
	models, err := ListModels()
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}

	for _, model := range models {
		if model.Name == name {
			return &model, nil
		}
	}

	return nil, fmt.Errorf("model %q not found", name)
}

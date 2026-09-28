package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

type Provider string

const (
	ProviderOpenAI Provider = "openai"
	ProviderAzure  Provider = "azure"
)

var ErrModelNotFound = errors.New("model not found")

type Model struct {
	Name        string   `json:"name"`
	Endpoint    string   `json:"endpoint"`
	Description string   `json:"description"`
	Provider    Provider `json:"provider"`
	APIKeyEnv   string   `json:"apiKeyEnv,omitempty"`
	APIKey      string   `json:"-"`
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
		if env := result.Models[index].APIKeyEnv; env != "" {
			result.Models[index].APIKey = os.Getenv(env)
		}
	}

	return result.Models, nil
}

type ModelCall interface {
	Call(ctx context.Context, messages []Message, tools []Tool) (Message, error)
	Embed(ctx context.Context, input string) ([]float64, error)
}

func NewModelClient(config Model) (ModelCall, error) {
	options := []option.RequestOption{option.WithBaseURL(config.Endpoint), option.WithMaxRetries(0)}
	switch {
	case config.Provider != ProviderOpenAI && config.Provider != ProviderAzure:
		return nil, fmt.Errorf("unsupported provider %q", config.Provider)
	case config.Provider == ProviderOpenAI || config.APIKeyEnv != "" || config.APIKey != "":
		options = append(options, option.WithAPIKey(config.APIKey))
	default:
		// azure.WithTokenCredential requires azure.WithEndpoint, which rewrites /openai/v1/ paths.
		credential, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, fmt.Errorf("create Azure credential: %w", err)
		}
		options = append(options, option.WithMiddleware(func(request *http.Request, next option.MiddlewareNext) (*http.Response, error) {
			token, err := credential.GetToken(request.Context(), policy.TokenRequestOptions{
				Scopes: []string{"https://cognitiveservices.azure.com/.default"},
			})
			if err != nil {
				return nil, fmt.Errorf("get Azure token: %w", err)
			}
			request.Header.Set("Authorization", "Bearer "+token.Token)
			return next(request)
		}))
	}
	return &OpenAIChatModel{Config: config, Client: openai.NewClient(options...)}, nil
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

	return nil, fmt.Errorf("%w: %q", ErrModelNotFound, name)
}

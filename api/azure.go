package api
import (
    "context"
    "fmt"
    "github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
    "github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/openai/openai-go/v3"
    "github.com/openai/openai-go/v3/option"
)
type AzureModel struct {
    Config Model
}

func (model *AzureModel) Call(ctx context.Context, prompt string) (string, error) {
    config := model.Config

    var authOption option.RequestOption
    if config.APIKeyEnv != "" || config.APIKey != "" {
        if config.APIKey == "" {
            return "", fmt.Errorf(
                "API key is missing or empty for model %q",
                config.Name,
            )
        }
        authOption = option.WithAPIKey(config.APIKey)
    } else {
        credential, err := azidentity.NewDefaultAzureCredential(nil)
        if err != nil {
            return "", fmt.Errorf("create Azure credential: %w", err)
        }
        token, err := credential.GetToken(ctx, policy.TokenRequestOptions{
            Scopes: []string{"https://cognitiveservices.azure.com/.default"},
        })
        if err != nil {
            return "", fmt.Errorf("get Azure token: %w", err)
        }
        authOption = option.WithAPIKey(token.Token)
    }

    client := OpenAIChatModel{
        Config: model.Config,
        Client: openai.NewClient(
            option.WithBaseURL(model.Config.Endpoint),
            authOption,
            option.WithMaxRetries(0),
        ),
    }
    return client.Call(ctx, prompt)
}
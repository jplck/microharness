package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	api "github.com/jplck/micro/api"
)

var listModelsCmd = &cobra.Command{
	Use:   "list-models",
	Short: "List available models",
	RunE: func(cmd *cobra.Command, args []string) error {
		models, err := api.ListModels()
		if err != nil {
			return fmt.Errorf("list models: %w", err)
		}

		for _, model := range models {
			fmt.Printf("Name: %s\n", model.Name)
			fmt.Printf("Endpoint: %s\n", model.Endpoint)
			fmt.Printf("Description: %s\n", model.Description)
			fmt.Printf("Provider: %s\n", model.Provider)
			fmt.Printf("API Key Env: %s\n", model.APIKeyEnv)
			fmt.Printf("API Key: %s\n", model.APIKey)
			fmt.Println()
		}

		return nil
	},
}

var callModelCmd = &cobra.Command{
	Use:   "call-model",
	Short: "Call a specific model",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) < 2 {
			return fmt.Errorf("usage: call-model <model-name> <prompt>")
		}

		modelName := args[0]
		prompt := args[1]

		model, err := api.GetModelByName(modelName)
		if err != nil {
			return fmt.Errorf("get model by name: %w", err)
		}

		client, err := api.NewModelClient(*model)
		if err != nil {
			return fmt.Errorf("create model client: %w", err)
		}

		tools := []api.Tool{
			{
				Name:        "get_time",
				Description: "Get the current time in a specific location",
				Parameters: []api.Parameter{
					{
						Name:        "location",
						Type:        api.String,
						Description: "The location to get the current time for",
						Required:    true,
					},
				},
				Execute: func(ctx context.Context, arguments json.RawMessage) (string, error) {
					var params struct {
						Location string `json:"location"`
					}
					if err := json.Unmarshal(arguments, &params); err != nil {
						return "", fmt.Errorf("invalid arguments: %w", err)
					}
					return fmt.Sprintf("Current time in %s: %s", params.Location, time.Now().Format(time.RFC3339)), nil
				},
			},
		}

		agent, err := api.CreateAgent(cmd.Context(), client, tools, "cli-agent", "Follow the instructions carefully.", "OBGXQCKYQV7NTMI4BJBMEMMUUK")
		if err != nil {
			return fmt.Errorf("create agent: %w", err)
		}

		response, err := agent.Execute(cmd.Context(), api.Message{Role: "user", Content: prompt})
		if err != nil {
			return fmt.Errorf("execute agent: %w", err)
		}
		fmt.Println("Response:", response)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(listModelsCmd)
	rootCmd.AddCommand(callModelCmd)
}

package cmd

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
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

var environmentSocket string

var environmentCreateAgentCmd = &cobra.Command{
	Use:   "create-agent <environment> <name> <model>",
	Short: "Create an agent within an environment",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		path := "/environments/" + url.PathEscape(args[0]) +
			"/agents/" + url.PathEscape(args[1]) +
			"?" + url.Values{"model": []string{args[2]}}.Encode()
		return runtimeRequest(cmd, http.MethodPost, path)
	},
}

var environmentCmd = &cobra.Command{
	Use:   "environment",
	Short: "Manage environments in the running service",
}

var environmentCreateCmd = &cobra.Command{
	Use:   "create <id>",
	Short: "Create an environment",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runtimeRequest(cmd, http.MethodPost,
			"/environments/"+url.PathEscape(args[0]))
	},
}

var environmentListCmd = &cobra.Command{
	Use:   "list",
	Short: "List active environments",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runtimeRequest(cmd, http.MethodGet, "/environments")
	},
}

func runtimeRequest(cmd *cobra.Command, method, path string) error {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", environmentSocket)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	request, err := http.NewRequestWithContext(
		cmd.Context(), method, "http://localhost"+path, nil,
	)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("contact runtime at %s: %w", environmentSocket, err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
		if err != nil {
			return fmt.Errorf("runtime returned %s; read response: %w", response.Status, err)
		}
		return fmt.Errorf("runtime returned %s: %s", response.Status, body)
	}
	if method == http.MethodPost {
		cmd.Println("Environment created.")
		return nil
	}
	_, err = io.Copy(cmd.OutOrStdout(), response.Body)
	return err
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve the API over a Unix socket",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, stop := signal.NotifyContext(
			cmd.Context(),
			os.Interrupt,
			syscall.SIGTERM,
		)
		defer stop()

		return api.ServeRuntime(ctx, "/tmp/micro.sock", "./data")
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
				Execute: api.JSONHandler(func(ctx context.Context, arguments struct {
					Location string `json:"location"`
				}) (string, error) {
					return fmt.Sprintf("Current time in %s: %s", arguments.Location, time.Now().Format(time.RFC3339)), nil
				}),
			},
		}

		env, err := api.NewAgentEnvironment(cmd.Context(), "./data", "cli-environment")
		if err != nil {
			return fmt.Errorf("create agent environment: %w", err)
		}
		defer env.Wait()

		agent, err := env.CreateAgent(cmd.Context(), client, tools, "cli-agent", "Follow the instructions carefully.", "", true)
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
	rootCmd.AddCommand(serveCmd)
	environmentCmd.PersistentFlags().StringVar(
		&environmentSocket, "socket", "/tmp/micro.sock", "Runtime Unix socket",
	)
	environmentCmd.AddCommand(environmentCreateCmd, environmentListCmd, environmentCreateAgentCmd)
	rootCmd.AddCommand(environmentCmd)
}

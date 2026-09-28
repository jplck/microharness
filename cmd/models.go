package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
		return runtimeRequest(cmd, http.MethodPost, path, nil)
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
			"/environments/"+url.PathEscape(args[0]), nil)
	},
}

var environmentListCmd = &cobra.Command{
	Use:   "list",
	Short: "List active environments",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runtimeRequest(cmd, http.MethodGet, "/environments", nil)
	},
}

var environmentMessageCmd = &cobra.Command{
	Use: "message <environment> <agent> <content>", Short: "Queue a message for an agent", Args: cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		sender, _ := cmd.Flags().GetString("sender")
		conversation, _ := cmd.Flags().GetString("conversation")
		replyTo, _ := cmd.Flags().GetString("reply-to")
		data, err := json.Marshal(api.Envelope{
			Source: "cli", Sender: sender, To: args[1], Content: args[2],
			ConversationID: conversation, ReplyTo: replyTo,
		})
		if err != nil {
			return err
		}
		return runtimeRequest(cmd, http.MethodPost, "/environments/"+url.PathEscape(args[0])+"/messages", bytes.NewReader(data))
	},
}

var environmentInboxCmd = &cobra.Command{
	Use: "inbox <environment> <agent>", Short: "Show pending messages and inbox errors", Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runtimeRequest(cmd, http.MethodGet, "/environments/"+url.PathEscape(args[0])+"/agents/"+url.PathEscape(args[1])+"/inbox", nil)
	},
}

var environmentRetryInboxCmd = &cobra.Command{
	Use: "retry-inbox <environment> <agent>", Short: "Resume a paused agent inbox", Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runtimeRequest(cmd, http.MethodPost, "/environments/"+url.PathEscape(args[0])+"/agents/"+url.PathEscape(args[1])+"/inbox/retry", nil)
	},
}

func runtimeRequest(cmd *cobra.Command, method, path string, body io.Reader) error {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", environmentSocket)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	request, err := http.NewRequestWithContext(
		cmd.Context(), method, "http://localhost"+path, body,
	)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
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
	written, err := io.Copy(cmd.OutOrStdout(), response.Body)
	if err == nil && method == http.MethodPost && written == 0 {
		cmd.Println("Request accepted.")
	}
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

		registry := api.BuiltinTools()
		env, err := api.NewAgentEnvironment(cmd.Context(), "./data", "cli-environment")
		if errors.Is(err, os.ErrExist) {
			env, err = api.LoadAgentEnvironment(cmd.Context(), "./data", "cli-environment", registry)
		}
		if err != nil {
			return fmt.Errorf("open agent environment: %w", err)
		}
		defer env.Wait()

		var agent *api.Agent
		for _, existing := range env.Agents {
			if existing.Name == "cli-agent" {
				agent = existing
				break
			}
		}
		if agent == nil {
			agent, err = env.CreateAgent(cmd.Context(), modelName, []api.Tool{registry["get_time"]}, "cli-agent", "Follow the instructions carefully.", true)
			if err != nil {
				return fmt.Errorf("create agent: %w", err)
			}
		} else if agent.ModelName != modelName {
			config, err := api.GetModelByName(modelName)
			if err != nil {
				return err
			}
			client, err := api.NewModelClient(*config)
			if err != nil {
				return err
			}
			agent.Client, agent.ModelName = client, modelName
			if err := env.Save(); err != nil {
				return err
			}
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
	environmentMessageCmd.Flags().String("sender", "cli", "External sender identity")
	environmentMessageCmd.Flags().String("conversation", "", "Conversation ID (generated when omitted)")
	environmentMessageCmd.Flags().String("reply-to", "", "Envelope ID being answered")
	environmentCmd.AddCommand(environmentMessageCmd, environmentInboxCmd, environmentRetryInboxCmd)
	rootCmd.AddCommand(environmentCmd)
}

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
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
			fmt.Printf("Name: %s\nEndpoint: %s\nDescription: %s\nProvider: %s\nAPI Key Env: %s\n\n",
				model.Name, model.Endpoint, model.Description, model.Provider, model.APIKeyEnv)
		}

		return nil
	},
}

var socketPath string

var environmentCreateAgentCmd = &cobra.Command{
	Use:   "create-agent <environment> <name> <model>",
	Short: "Create an agent within an environment",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		instructions, _ := cmd.Flags().GetString("instructions")
		path := agentPath(args[0], args[1]) + "?" + url.Values{
			"model":        []string{args[2]},
			"instructions": []string{instructions},
		}.Encode()
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
		return runtimeRequest(cmd, http.MethodGet, agentPath(args[0], args[1])+"/inbox", nil)
	},
}

var environmentRetryInboxCmd = &cobra.Command{
	Use: "retry-inbox <environment> <agent>", Short: "Resume a paused agent inbox", Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runtimeRequest(cmd, http.MethodPost, agentPath(args[0], args[1])+"/inbox/retry", nil)
	},
}

func agentPath(environment, agent string) string {
	return "/environments/" + url.PathEscape(environment) + "/agents/" + url.PathEscape(agent)
}

func runtimeRequest(cmd *cobra.Command, method, path string, body io.Reader) error {
	client := newRuntimeClient(socketPath)
	defer client.CloseIdleConnections()
	data, err := requestRuntime(cmd.Context(), client, method, path, body)
	if err != nil {
		return err
	}
	if method == http.MethodPost && len(data) == 0 {
		cmd.Println("Request accepted.")
		return nil
	}
	_, err = cmd.OutOrStdout().Write(data)
	return err
}

func newRuntimeClient(socket string) *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}
}

func requestRuntime(ctx context.Context, client *http.Client, method, path string, body io.Reader) ([]byte, error) {
	request, err := http.NewRequestWithContext(
		ctx, method, "http://localhost"+path, body,
	)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("contact runtime (start micro serve): %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
		if err != nil {
			return nil, fmt.Errorf("runtime returned %s; read response: %w", response.Status, err)
		}
		return nil, fmt.Errorf("runtime returned %s: %s", response.Status, body)
	}
	return io.ReadAll(response.Body)
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve the API over a Unix socket",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return api.ServeRuntime(ctx, socketPath, "./data")
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&socketPath, "socket", "/tmp/micro.sock", "Runtime Unix socket")
	rootCmd.AddCommand(listModelsCmd)
	rootCmd.AddCommand(serveCmd)
	environmentCreateAgentCmd.Flags().String("instructions", "", "Agent-specific instructions added to the default system prompt")
	environmentCmd.AddCommand(environmentCreateCmd, environmentListCmd, environmentCreateAgentCmd)
	environmentMessageCmd.Flags().String("sender", "cli", "External sender identity")
	environmentMessageCmd.Flags().String("conversation", "", "Conversation ID (generated when omitted)")
	environmentMessageCmd.Flags().String("reply-to", "", "Envelope ID being answered")
	environmentCmd.AddCommand(environmentMessageCmd, environmentInboxCmd, environmentRetryInboxCmd)
	rootCmd.AddCommand(environmentCmd)
}

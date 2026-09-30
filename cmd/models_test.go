package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	api "github.com/jplck/micro/api"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestExternalPluginOptionsRemoved(t *testing.T) {
	if serveCmd.Flags().Lookup("plugins") != nil {
		t.Fatal("serve still accepts an external plugin directory")
	}

	for _, command := range rootCmd.Commands() {
		if command.Name() == "plugins" {
			t.Fatal("external plugin management command is still registered")
		}
	}
}

func TestCLIMessageDeliveryCommands(t *testing.T) {
	directory, err := os.MkdirTemp(".", ".cli-message-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	previousSocket := socketPath
	socketPath = filepath.Join(directory, "runtime.sock")
	t.Cleanup(func() { socketPath = previousSocket })
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	var handler http.HandlerFunc
	var handlerMutex sync.RWMutex
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerMutex.RLock()
		defer handlerMutex.RUnlock()
		handler(w, r)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })

	for _, test := range []struct {
		name   string
		cmd    *cobra.Command
		args   []string
		flags  map[string]string
		method string
		path   string
		status int
	}{
		{"steering", environmentMessageCmd, []string{"team", "worker", "Change direction"}, map[string]string{"sender": "alice", "steer": "run-1"}, "POST", "/environments/team/messages", 202},
		{"status", environmentMessageStatusCmd, []string{"team", "message/id"}, nil, "GET", "/environments/team/messages/message%2Fid", 200},
		{"retract", environmentRetractMessageCmd, []string{"team", "message-1"}, map[string]string{"sender": "alice"}, "POST", "/environments/team/messages/message-1/retract", 200},
		{"default sender", environmentRetractMessageCmd, []string{"team", "message-1"}, nil, "POST", "/environments/team/messages/message-1/retract", 200},
		{"too late", environmentRetractMessageCmd, []string{"team", "message-1"}, nil, "POST", "/environments/team/messages/message-1/retract", 409},
		{"not owner", environmentRetractMessageCmd, []string{"team", "message-1"}, nil, "POST", "/environments/team/messages/message-1/retract", 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.cmd.Flags().VisitAll(func(flag *pflag.Flag) {
				value := flag.Value.String()
				t.Cleanup(func() { flag.Value.Set(value) })
				flag.Value.Set(flag.DefValue)
			})
			for name, value := range test.flags {
				if err := test.cmd.Flags().Set(name, value); err != nil {
					t.Fatal(err)
				}
			}
			var calls atomic.Int32
			handlerMutex.Lock()
			handler = func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != test.method || r.URL.EscapedPath() != test.path {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				if r.Method == http.MethodPost {
					var envelope api.Envelope
					if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
						t.Error(err)
					}
					sender := test.flags["sender"]
					if sender == "" {
						sender = "cli"
					}
					if envelope.Source != "cli" || envelope.Sender != sender {
						t.Errorf("wrong ownership: %+v", envelope)
					}
					if test.cmd == environmentMessageCmd && (envelope.Steer != "run-1" || envelope.To != "worker" || envelope.Content != "Change direction") {
						t.Errorf("wrong steering envelope: %+v", envelope)
					}
				}
				w.WriteHeader(test.status)
				if test.status >= 400 {
					io.WriteString(w, "retraction rejected")
				} else {
					io.WriteString(w, `{"id":"message-1","status":"retracted"}`)
				}
			}
			handlerMutex.Unlock()
			var output bytes.Buffer
			test.cmd.SetOut(&output)
			t.Cleanup(func() { test.cmd.SetOut(nil) })
			test.cmd.SetContext(t.Context())
			t.Cleanup(func() { test.cmd.SetContext(context.Background()) })
			if err := test.cmd.Args(test.cmd, test.args); err != nil {
				t.Fatal(err)
			}
			err := test.cmd.RunE(test.cmd, test.args)
			if test.status >= 400 {
				if err == nil || !strings.Contains(err.Error(), "retraction rejected") || !strings.Contains(err.Error(), http.StatusText(test.status)) {
					t.Fatalf("missing actionable error: %v", err)
				}
			} else if err != nil || !strings.Contains(output.String(), "message-1") {
				t.Fatalf("missing result: %s, %v", output.String(), err)
			}
			if calls.Load() != 1 {
				t.Fatalf("request count = %d", calls.Load())
			}
		})
	}
}

package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDeleteAgentPersistenceAndFiles(t *testing.T) {
	for _, erase := range []bool{false, true} {
		for _, state := range []string{"idle", "queued", "paused"} {
			t.Run(fmt.Sprintf("erase=%t/%s", erase, state), func(t *testing.T) {
				ctx, root, registry := setupPersistenceTest(t)
				env, err := NewAgentEnvironment(ctx, root, "team")
				if err != nil {
					t.Fatal(err)
				}
				defer env.Close()
				agent, err := env.CreateAgent(ctx, "test-model", nil, "first", "", true)
				if err != nil {
					t.Fatal(err)
				}
				other, err := env.CreateAgent(ctx, "test-model", nil, "other", "", false)
				if err != nil {
					t.Fatal(err)
				}
				sessionPath, err := agent.Session.path()
				if err != nil {
					t.Fatal(err)
				}
				pluginPath := filepath.Join(env.DataRoot, "plugins", "first-private")
				if err := os.MkdirAll(filepath.Join(pluginPath, "bin"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(pluginPath, "bin", "echo"), []byte("private plugin"), 0o600); err != nil {
					t.Fatal(err)
				}
				agent.plugins = []ownedPlugin{{Path: "first-private/bin/echo"}}
				memoryPath := filepath.Join(env.DataRoot, "memory.json")
				if err := os.WriteFile(memoryPath, []byte(`[]`), 0o600); err != nil {
					t.Fatal(err)
				}
				if state != "idle" {
					if _, err := env.Message(ctx, Envelope{Sender: "user", To: "first", Content: "Pending work"}); err != nil {
						t.Fatal(err)
					}
				}
				if state == "paused" {
					agent.InboxError = "failed request"
				}
				if err := env.saveLocked(); err != nil {
					t.Fatal(err)
				}
				if err := env.DeleteAgent(ctx, "first", erase); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(env.AgentNames(), []string{"other"}) || env.InitialAgent != other {
					t.Fatal("deletion did not preserve the other agent and promote the default")
				}
				for _, path := range []string{sessionPath, pluginPath} {
					_, err := os.Stat(path)
					if erase && !errors.Is(err, os.ErrNotExist) || !erase && err != nil {
						t.Fatalf("unexpected retained file state for %s: %v", path, err)
					}
				}
				if _, err := os.Stat(memoryPath); err != nil {
					t.Fatal("shared memory removed:", err)
				}
				loaded, err := LoadAgentEnvironment(ctx, root, "team", registry)
				if err != nil {
					t.Fatal(err)
				}
				if len(loaded.Agents) != 1 || loaded.InitialAgent.Name != "other" {
					t.Fatal("deletion did not survive reload")
				}
				if _, err := env.Message(ctx, Envelope{Sender: "user", To: "first", Content: "New work"}); !errors.Is(err, ErrAgentNotFound) {
					t.Fatal("deleted agent still receives messages:", err)
				}
				if err := agent.executeEnvelope(ctx, Envelope{ID: "stale", Content: "New work"}); !errors.Is(err, ErrAgentNotFound) {
					t.Fatal("deleted agent can still execute:", err)
				}
				if err := env.DeleteAgent(ctx, "other", false); err != nil {
					t.Fatal(err)
				}
				loaded, err = LoadAgentEnvironment(ctx, root, "team", registry)
				if err != nil || len(loaded.Agents) != 0 || loaded.InitialAgent != nil {
					t.Fatal("last-agent deletion did not clear the default:", err)
				}
				recreated, err := env.CreateAgent(ctx, "test-model", nil, "first", "", false)
				if err != nil || recreated == agent || recreated.Session.SessionID == agent.Session.SessionID || env.InitialAgent != recreated {
					t.Fatal("recreating the name did not create a fresh agent:", err)
				}
			})
		}
	}
}

func TestDeleteAgentGuardsAndRollback(t *testing.T) {
	ctx, root, _ := setupPersistenceTest(t)
	env, err := NewAgentEnvironment(ctx, root, "team")
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	agent, err := env.CreateAgent(ctx, "test-model", nil, "worker", "", true)
	if err != nil {
		t.Fatal(err)
	}
	agent.activeEnvelopeID = "active"
	if err := env.DeleteAgent(ctx, "worker", true); !errors.Is(err, ErrAgentRunning) {
		t.Fatal("active worker was not blocked:", err)
	}
	agent.activeEnvelopeID = ""
	agent.runMu.Lock()
	err = env.DeleteAgent(ctx, "worker", true)
	agent.runMu.Unlock()
	if !errors.Is(err, ErrAgentRunning) {
		t.Fatal("active execution was not blocked:", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := env.DeleteAgent(cancelled, "worker", true); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled deletion accepted:", err)
	}
	if err := env.DeleteAgent(ctx, "missing", false); !errors.Is(err, ErrAgentNotFound) {
		t.Fatal("missing agent accepted:", err)
	}
	originalRoot := env.DataRoot
	env.DataRoot = filepath.Join(root, "blocked")
	if err := os.WriteFile(env.DataRoot, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = env.DeleteAgent(ctx, "worker", true)
	env.DataRoot = originalRoot
	if err == nil || len(env.Agents) != 1 || env.Agents[0] != agent || env.InitialAgent != agent {
		t.Fatal("failed persistence did not restore live state:", err)
	}
	if err := agent.Session.Load(); err != nil {
		t.Fatal("failed deletion erased session:", err)
	}
	env.Close()
	if err := env.DeleteAgent(ctx, "worker", true); !errors.Is(err, ErrEnvironmentClosed) {
		t.Fatal("closed environment accepted deletion:", err)
	}
}

func TestDeleteAgentStopsWorker(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(fmt.Sprintf("paused=%t", paused), func(t *testing.T) {
			ctx, root, _ := setupPersistenceTest(t)
			env, err := NewAgentEnvironment(ctx, root, "team")
			if err != nil {
				t.Fatal(err)
			}
			defer env.Close()
			agent, err := env.CreateAgent(ctx, "test-model", nil, "worker", "", true)
			if err != nil {
				t.Fatal(err)
			}
			if paused {
				agent.Inbox = []Envelope{{ID: "pending"}}
				agent.InboxError = "paused"
			}
			if err := env.Start(ctx); err != nil {
				t.Fatal(err)
			}
			if err := env.DeleteAgent(ctx, "worker", true); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				env.workerWG.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("deleted agent's worker did not exit")
			}
		})
	}
}

func TestDeleteAgentCleanupFailure(t *testing.T) {
	ctx, root, registry := setupPersistenceTest(t)
	env, err := NewAgentEnvironment(ctx, root, "team")
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	agent, err := env.CreateAgent(ctx, "test-model", nil, "worker", "", true)
	if err != nil {
		t.Fatal(err)
	}
	path, err := agent.Session.path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "blocked"), []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := env.DeleteAgent(ctx, "worker", true); !errors.Is(err, ErrAgentDataCleanup) {
		t.Fatal("cleanup failure was not reported explicitly:", err)
	}
	loaded, err := LoadAgentEnvironment(ctx, root, "team", registry)
	if err != nil || len(loaded.Agents) != 0 {
		t.Fatal("cleanup failure resurrected agent:", err)
	}
}

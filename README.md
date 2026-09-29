# Micro

## Terminal interface

Start the runtime in one terminal:

```sh
go run main.go serve
```

Then open the Bubble Tea interface in another terminal, from the directory
containing your `models.json`:

```sh
go run main.go
```

`go run main.go tui` opens the same interface. The TUI connects to the runtime;
it does not start a second runtime or process agent work itself. For a custom
socket, pass `--socket /path/to/micro.sock` to both commands.

- Use arrows to select an environment or agent and Enter to open it.
- Press `n` to create an environment or agent. Agent creation includes a model
  selector, optional multiline instructions, and a tool checklist.
- Press `e` on the agent list to edit its model, instructions, and tools using
  the same form as creation, prefilled with its current settings.
  In the tools field, Up/Down selects a tool and Space toggles its checkbox;
  Ctrl+S saves and Esc cancels. Tool descriptions appear below the checklist.
  Memory and messaging tools are always enabled. Optional choices come from
  the running server's registry; clearing all choices keeps the automatic tools.
  Changes are persisted and require an idle agent with an empty inbox, including
  no paused work. The name is fixed and session history is preserved; editing
  instructions updates the session's system prompt.
- Press `s` on an agent, its inbox, or its live session to compose a message, including optional
  conversation and reply IDs.
- Press `v` on an agent or its inbox to open its live session: messages, tool
  calls, tool results, and completed responses. It refreshes every second and
  follows the latest activity. Scroll up to read history; End resumes following.
  Esc returns to the agent list and stops polling. This shows saved messages,
  not token-by-token output while the model is generating a response.
  Composing a message pauses polling; sending or cancelling returns to the live
  session and resumes updates. Failed sends keep your draft for correction or retry.
- Press `m` from a list to view configured models, `r` to refresh, and `t` in an
  inbox to retry paused work. Inbox views show pending messages, not completed
  session output. Lists and inboxes are refreshed manually.
- In forms, Tab/Shift+Tab changes fields, Left/Right selects a model, Ctrl+S
  submits, and Esc cancels. Outside forms, Esc goes back and `q` quits.
  Ctrl+C always quits the interface without stopping the runtime.

The interface requires a terminal of at least 40 columns by 22 rows. All CLI
subcommands remain available for scripts.

## Agent messaging

Start the runtime, then use another terminal to create agents and send work:

```sh
go run main.go serve
```

```sh
go run main.go environment create team
go run main.go environment create-agent team coordinator gpt-5.4-mini
go run main.go environment create-agent team researcher gpt-5.4-mini --instructions "Research technical questions and report concise findings."
go run main.go environment message team coordinator "Message researcher to investigate this task."
go run main.go environment inbox team coordinator
go run main.go environment inbox team researcher
```

`create-agent --instructions` adds agent-specific instructions to the default
system prompt and persists them across restarts. Omitting the flag keeps the
default prompt. This creates a new agent; it does not update an existing agent.

`message` prints the accepted envelope ID, not the recipient's answer. The agent's
conversation and final output are saved in its session file. Each agent receives
`list_agents` and `message` tools automatically, including after reload.

The model-facing `message` tool accepts `to`, `content`, and optional `reply_to`.
The runtime supplies the sender identity and inherits the current conversation
ID. Agents send replies explicitly using the incoming sender and envelope ID;
final model output is not automatically sent back as another message.

CLI messages also support `--sender`, `--conversation`, and `--reply-to`.

## Delivery and recovery

- Sending persists an envelope before returning its ID. An empty `To` in the Go
  API or HTTP API routes to the environment's initial agent.
- One worker per agent processes its inbox in FIFO order. Sending never waits
  for another agent's answer, and different agents can work concurrently.
- `serve` starts workers for restored and newly created agents. Interrupted
  messages remain queued and resume after restart.
- Model or persistence failures pause that agent's inbox without discarding it.
  Inspect the error with `environment inbox`, fix the cause, then run:

```sh
go run main.go environment retry-inbox team researcher
```

Completed session turns are recognized when acknowledging an inbox message is
interrupted. Persisted tool results are reused when resuming incomplete turns.
This is not an exactly-once guarantee: a crash after a tool side effect but before
its result is saved can repeat that effect. Side-effecting tools should support
idempotency. Repeated IDs are deduplicated while an envelope remains queued.

Use one process per environment data directory.

## Embedding in Go

Create or load an environment and its agents, then call `env.Start(ctx)` to
start workers. Submit `env.Message(ctx, api.Envelope{...})`, inspect with
`env.Inbox(name)`, and use `env.RetryInbox(ctx, name)` for paused work. Call
`env.Close()` to cancel workers, preserve unfinished messages, and wait for them
to exit.

The Unix-socket API exposes:

- `GET /tools`: registered optional tool names and descriptions.
- `POST /environments/{id}/agents/{name}?model=...&tool=get_time`: create an
  agent with optional repeated `tool` query parameters and `instructions`.
- `PUT /environments/{id}/agents/{name}?model=...&instructions=...&tool=get_time`:
  replace model, instructions, and optional tools while preserving identity and
  session history. Omitted instructions/tools clear those settings. Returns 204
  on success, 400 for invalid settings, or 409 when the agent is busy.
- `PUT /environments/{id}/agents/{name}/tools`: replace optional tool assignments
  with a JSON array of registered names, such as `["get_time"]` or `[]`.
  Returns 204 on success, 400 for invalid tools, or 409 when the agent is busy.
- `GET /environments/{id}/agents`: agent names, models, custom instructions,
  optional `tools`, pending-message counts, and any inbox error.
- `GET /environments/{id}/agents/{name}/session`: the latest committed session
  snapshot (`messages`), available even while the agent is processing work.
- `POST /environments/{id}/messages`: an `Envelope` JSON object using the Go field
  names (`Source`, `Sender`, `To`, `Content`, `ConversationID`, `ReplyTo`, optional
  `ID`); returns HTTP 202 with `id` and `status`.
- `GET /environments/{id}/agents/{name}/inbox`: pending `messages` and an optional
  `error` describing why the inbox is paused.
- `POST /environments/{id}/agents/{name}/inbox/retry`: resume a paused inbox.

The socket is owner-only. External adapters must authenticate their own callers;
the HTTP API reserves `Source: "agent"` for internal tool calls. Phone-messenger
and cron adapters themselves are not included.
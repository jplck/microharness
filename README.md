# Micro

## Agent messaging

Start the runtime, then use another terminal to create agents and send work:

```sh
go run main.go serve
```

```sh
go run main.go environment create team
go run main.go environment create-agent team coordinator gpt-5.4-mini
go run main.go environment create-agent team researcher gpt-5.4-mini
go run main.go environment message team coordinator "Message researcher to investigate this task."
go run main.go environment inbox team coordinator
go run main.go environment inbox team researcher
```

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

Use one process per environment data directory. The standalone `call-model`
command remains a direct model call; it does not start inbox workers. Use the
runtime messaging commands for automatic agent-to-agent delivery.

## Embedding in Go

Create or load an environment and its agents, then call `env.Start(ctx)` to
start workers. Submit `env.Message(ctx, api.Envelope{...})`, inspect with
`env.Inbox(name)`, and use `env.RetryInbox(ctx, name)` for paused work. Call
`env.Close()` to cancel workers, preserve unfinished messages, and wait for them
to exit. `env.Wait()` only waits for memory embedding work.

The Unix-socket API exposes:

- `POST /environments/{id}/messages`: an `Envelope` JSON object using the Go field
  names (`Source`, `Sender`, `To`, `Content`, `ConversationID`, `ReplyTo`, optional
  `ID`); returns HTTP 202 with `id` and `status`.
- `GET /environments/{id}/agents/{name}/inbox`: pending `messages` and an optional
  `error` describing why the inbox is paused.
- `POST /environments/{id}/agents/{name}/inbox/retry`: resume a paused inbox.

The socket is owner-only. External adapters must authenticate their own callers;
the HTTP API reserves `Source: "agent"` for internal tool calls. Phone-messenger
and cron adapters themselves are not included.
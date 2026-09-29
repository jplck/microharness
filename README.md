# Micro

## Terminal interface

Build the bundled tool plugin, then start the runtime in one terminal:

```sh
go build -o plugins/bin/builtin ./cmd/micro-tools
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
  Each tool has independent Use and Assign checkboxes: Use lets the agent call
  it; Assign lets `create_agent` grant it to a child without needing Use.
  In the tools field, Up/Down selects a tool, Left/Right selects Use or Assign,
  and Space toggles that checkbox;
  Ctrl+S saves and Esc cancels. Tool descriptions appear below the checklist.
  Memory and messaging tools are always enabled. Optional choices come from
  the running server's registry; clearing all choices keeps the automatic tools.
  Selecting `create_agent` under Use or Assign reveals a Child models checklist
  populated from `models.json`. Tab to it, use Up/Down to select a row, and Space
  to toggle each permitted model. This list is separate from the agent's own
  model; selecting none blocks child creation. Small terminals show fewer rows
  and omit tool descriptions while this checklist is visible.
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
`list_agents`, `agent_status`, and `message` tools automatically, including after reload.

The model-facing `message` tool accepts `to`, `content`, and optional `reply_to`.
The runtime supplies the sender identity and inherits the current conversation
ID. Agents send replies explicitly using the incoming sender and envelope ID;
final model output is not automatically sent back as another message.
The tool's receipt includes a `recipient` status snapshot in addition to the
accepted ID. `agent_status` accepts `{"name":"timekeeper"}` and reports `idle`,
`queued`, `running`, or `paused`, the pending count (including active work), the
active envelope ID when running, and any inbox error. These are agent processing
states, not proof that a delegated task has been completed. The TUI displays them
in agent lists and inbox views; refresh those views with `r`.

CLI messages also support `--sender`, `--conversation`, and `--reply-to`.

## Autonomous agent creation

Enable the optional `create_agent` tool in an agent's create/edit tool checklist.
It is available in `DefaultTools()` but is not assigned automatically.
It uses the shared agent-creation path in
the caller's environment, including validation, persistence, and worker startup.

The tool accepts `name`, `model` (a configured model name), optional
`instructions`, and an optional `tools` array. Every requested tool must be in
the caller's Assign list; Use alone does not permit delegation. The tool's
description includes the allowed names and descriptions. The model parameter's
schema lists only configured models selected in the caller's Child models list;
the runtime enforces that list even if a model returns an unlisted name.
For example, with Assign enabled for `get_time` and `gpt-5.4-mini` selected as
a child model:

```json
{"name":"researcher","model":"gpt-5.4-mini","instructions":"Research assigned questions and message the sender with your findings.","tools":["get_time"]}
```

After creation, the parent uses `message` to delegate work to the returned name.
Children receive automatic memory and messaging tools plus the explicitly
requested usable tools, but no Assign permissions. Omitting `tools` or using
`[]` grants no optional tools. Granting Use of `create_agent` to a child does
not let that child grant optional tools of its own. Unknown, unauthorized, or
duplicate tool names reject the whole creation request. Existing agents have
no Assign permissions until explicitly configured; Use is not upgraded to Assign.
Children granted Use of `create_agent` inherit a copy of the parent's child-model
allowlist, but still receive no Assign tool permissions. They may create children
using those models with automatic tools only. Model limits do not change the
agent's own model and are not automatically widened when models are added to
configuration. Updating a parent's limits does not change existing children's
copies. Existing agents without a saved allowlist cannot create children until
models are explicitly selected, even if they already have Use of `create_agent`.
Creating an agent does not itself enqueue a task or change the initial agent.
Duplicate names fail rather than replacing existing agents. Only grant this
tool to agents trusted to create persistent workers; there is no agent-count
or spending limit enforced by this tool.

For Go callers, assign `api.CreateAgentTool()` through `env.CreateAgentWithModels` or
`env.SetAgentTools`, and use `api.DefaultTools()` as the registry when loading
saved environments. Add custom tool definitions to that registry as needed.
The environment binds each tool to its caller when it is assigned.
`CreateAgent` and `UpdateAgent` accept optional trailing assignable `Tool` values,
independent of the usable `[]Tool` argument. `CreateAgentWithModels` and
`UpdateAgentWithModels` additionally accept an explicit `[]string` child-model
allowlist before the variadic assignable tools. The older methods supply an empty
allowlist. Full updates replace all permissions; `SetAgentTools` changes only Use
and preserves Assign and child-model permissions.
All assignable tools must also be in the registry used to restore the environment.

## Tool architecture

All runtime catalogue tools execute as binary plugins, including memory,
messaging, agent creation, time, and tool creation. The built-ins ship together
in one executable, built from [cmd/micro-tools/main.go](cmd/micro-tools/main.go).
There is no Go `.so` loading or Python runner.

- [toolplugin/plugin.go](toolplugin/plugin.go) is the stdlib-only SDK and the
  authoritative version-1 wire contract: `Definition`, `Parameter`, `Manifest`,
  `Request`, `Response`, `Tool`, `Handler`, and `Serve`.
- [toolplugin/builtin/tools.go](toolplugin/builtin/tools.go) owns built-in tool
  definitions and plugin-side handlers. Stateful operations use a private,
  per-invocation Unix socket. Only that built-in's operation is exposed; the
  host supplies caller identity and conversation metadata and checks grants.
  External and agent-created plugins receive no host socket.
- [api/plugins.go](api/plugins.go) contains the central subprocess launcher for
  builds, discovery, and calls, and the host operation bridge. Agent state,
  queues, memory, persistence, and authorization remain in the runtime.
- [api/plugin_catalog.go](api/plugin_catalog.go) validates manifests and takes
  immutable executable snapshots. [api/tool_catalog.go](api/tool_catalog.go)
  retains Use/Assign selection and caller binding. Tools are refreshed before
  each invocation and model call, without changing an agent's grants.

Existing built-in names and permission fields are unchanged. `DefaultTools()`
now returns binary-backed definitions, including optional `create_tool`.
Direct Go callers must also build the bundled executable. Trusted custom Go
`Tool` values remain supported by the Go API, but are not a runtime plugin
installation mechanism.

## Plugin contract

Start from [examples/echo/main.go](examples/echo/main.go). Define a
`toolplugin.Tool` with a `Definition` and `Call func(json.RawMessage)
(string, error)`, optionally using `toolplugin.Handler` for typed arguments.
Call `toolplugin.Serve` from `main`; print startup failures to stderr and exit
nonzero. The SDK supplies both commands below. A plugin does not import `api`.

`plugin describe` takes no input and returns exactly one JSON object:

```json
{"protocol_version":1,"tools":[{"name":"echo","description":"Return text","parameters":[{"name":"input","type":"string","description":"Text","required":true}]}]}
```

`plugin call echo` reads exactly one JSON object from stdin until EOF:

```json
{"protocol_version":1,"arguments":{"input":"hello"}}
```

It writes exactly one JSON response to stdout, with exit status zero:

```json
{"protocol_version":1,"result":"hello"}
```

An application error uses `{"protocol_version":1,"result":"","error":"message"}`.
Nonzero exits, invalid JSON, unsupported versions, output overflow, and timeouts
are tool failures. `code` is reserved for host-operation error classification.
Stdout is protocol-only; diagnostics belong on stderr. There is no streaming,
long-lived plugin process, or plugin-selected caller identity.

Names match `[a-z][a-z0-9_]{0,63}`. A manifest contains 1-64 distinct tools with
nonempty descriptions (max 8192 bytes). Parameters use `string`, `integer`,
`number`, `boolean`, or `array` with primitive `items`. `enum` is supported for
strings. Nested object schemas are not supported in this version. Only the
bundled plugin may declare `automatic: true`. Duplicate names reject discovery.

## Install and reload

Build binaries for the runtime's OS and architecture. For the example:

```sh
go build -o plugins/bin/.echo-new ./examples/echo
mv plugins/bin/.echo-new plugins/bin/echo
go run main.go plugins reload
```

The runtime scans `plugins/bin` at startup; `serve --plugins <directory>` changes
the additional-plugin directory. `MICRO_BUILTIN_PLUGIN` overrides the bundled
executable path independently. Hidden files and directories are ignored. Other
entries must be executable regular files. Build to a hidden temporary filename
and rename into place, never overwrite a live executable in place.

Reload is also available as `POST /tools/reload`. It validates the whole candidate
catalogue before publishing it; failures preserve the active catalogue. New
tools appear in the agent form's Use/Assign picker when reopened, but are never
automatically granted. Replacements apply to subsequent calls; invocations
already holding a snapshot keep the old binary. Snapshots are retained until
runtime shutdown. Removing a binary and reloading removes its tools from calls
without erasing grants. Before restarting, restore missing binaries or remove
their grants: startup fails if saved agents require absent registered tools.

## Agent-created plugins

Enable Use for `create_tool` on a trusted agent. It accepts `name`, `source`
(a complete Go main package, max 64 KiB), `test_arguments` (a JSON object encoded
as a string), and `expected_output` (the exact result string). It builds against
the runtime's embedded copy of the same SDK, validates `describe`, and runs a
sample `call` before registering exactly one tool. The example above works
unchanged as `source`. Only the standard library and the SDK are available;
module downloads, CGO, workspace overrides, and automatic toolchain downloads
are disabled. A local Go 1.25+ toolchain is required for creation.

The creating agent can invoke its tool immediately, even in the same tool-call
batch. Definitions, source, and binaries persist under the environment's
`plugins` directory. Saved agent state references the installed directory;
startup runs `describe`, not the sample call, to restore its schema. Missing or
invalid private binaries fail restoration. Tools remain private and cannot be
assigned to children. Editing an agent preserves them; unchecking Use for
`create_tool` disables both creation and use of its private plugins. Re-enabling
it restores access. Individual replacement, deletion, and sharing of private
plugins are not implemented. Failed samples and failed state saves do not
register a tool, though any effects of executing the sample cannot be undone.

## Execution and sandboxing

**Plugins are not sandboxed yet.** Discovery, compilation, sample tests, and
normal calls execute with the runtime user's OS access. Grant creation only to
trusted agents and install only trusted binaries. A restricted environment and
temporary working directory are not security boundaries; plugins can access
files, networks, and processes, including secrets on disk.

Calls use a fresh temporary working directory and receive only PATH and LANG,
plus the invocation socket for stateful built-ins. Compilation also receives
HOME and the Go build-cache path. Model API key environment variables are not
passed. Requests are limited to 128 KiB; stdout and stderr each to 1 MiB.
Discovery has a 10-second timeout, calls 30 seconds, compilation 90 seconds,
and the enclosing `create_tool` operation 120 seconds. Cancellation kills the
direct process; there are no memory, CPU, or descendant-process guarantees.

Nono is intentionally not integrated yet. The central launcher is the place to
wrap all execution stages with host-selected policies later, with no plugin
contract changes. The host socket still needs separate authorization because
sandbox restrictions on a child do not restrict operations performed by its host.

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

When an inbox pauses, the runtime queues an `agent_blocked` notification to the
sender of each blocked agent-origin envelope, including requests behind the failed
one. New agent requests sent to an already paused inbox are also notified. The
notification uses `Source: "runtime"`, `ReplyTo` matching the blocked request ID,
and the original conversation ID. Its JSON content identifies the recipient,
failed envelope, affected request, and error. The original requests stay queued;
there is no automatic retry or cancellation.

Notifications and their deduplication markers are saved with the paused state.
Restarting does not resend notices already saved, including notices already
consumed by the requester. An explicit retry starts a new attempt and can produce
new notices if it fails again. Runtime notices never produce failure-notification
loops. A paused requester receives notices in its queue but cannot process them
until it too is retried. External users inspect failures through the inbox/API;
runtime notices are delivered only to agent senders. Persistence failures can
prevent notices from being saved; these failures are logged, and notification
delivery is retried on restart or a later send to the paused inbox.

Completed session turns are recognized when acknowledging an inbox message is
interrupted. Persisted tool results are reused when resuming incomplete turns.
This is not an exactly-once guarantee: a crash after a tool side effect but before
its result is saved can repeat that effect. Side-effecting tools should support
idempotency. Repeated IDs are deduplicated while an envelope remains queued.

Use one process per environment data directory.

## Embedding in Go

Create or load an environment and its agents, then call `env.Start(ctx)` to
start workers. Submit `env.Message(ctx, api.Envelope{...})`, inspect with
`env.Inbox(name)` or `env.Status(name)`, and use `env.RetryInbox(ctx, name)` for paused work. Call
`env.Close()` to cancel workers, preserve unfinished messages, and wait for them
to exit.

The Unix-socket API exposes:

- `GET /tools`: registered optional tool names and descriptions.
- `POST /environments/{id}/agents/{name}?model=...&tool=get_time`: create an
  agent with optional repeated `tool` (Use) and `assignable_tool` (Assign)
  query parameters, repeated `allowed_model` child-model permissions, and `instructions`.
- `PUT /environments/{id}/agents/{name}?model=...&instructions=...&tool=get_time`:
  replace model, instructions, Use tools, and Assign tools while preserving identity
  and session history. Repeated `assignable_tool` parameters set Assign permissions.
  Repeated `allowed_model` parameters replace child-model permissions.
  Omitted instructions/tools/assignable tools/allowed models clear those settings. Returns 204
  on success, 400 for invalid settings, or 409 when the agent is busy.
- `PUT /environments/{id}/agents/{name}/tools`: replace optional tool assignments
  with a JSON array of registered names, such as `["get_time"]` or `[]`.
  This changes only Use permissions and preserves Assign and child-model permissions.
  Returns 204 on success, 400 for invalid tools, or 409 when the agent is busy.
- `GET /environments/{id}/agents`: agent names, models, custom instructions,
  optional `tools`, `assignable_tools`, `allowed_models`, processing `status`,
  `active_envelope_id` when running, pending-message counts, and any inbox error.
- `GET /environments/{id}/agents/{name}/status`: name, processing status, pending
  count, active envelope ID when running, and any inbox error.
- `GET /environments/{id}/agents/{name}/session`: the latest committed session
  snapshot (`messages`), available even while the agent is processing work.
- `POST /environments/{id}/messages`: an `Envelope` JSON object using the Go field
  names (`Source`, `Sender`, `To`, `Content`, `ConversationID`, `ReplyTo`, optional
  `ID`); returns HTTP 202 with `id` and `status`. Sources `agent` and `runtime`
  are reserved and rejected from external HTTP clients.
- `GET /environments/{id}/agents/{name}/inbox`: pending `messages`, processing
  `status`, active envelope ID when running, and an optional `error` describing
  why the inbox is paused.
- `POST /environments/{id}/agents/{name}/inbox/retry`: resume a paused inbox.

The socket is owner-only. External adapters must authenticate their own callers;
the HTTP API reserves `Source: "agent"` for internal tool calls. Phone-messenger
and cron adapters themselves are not included.
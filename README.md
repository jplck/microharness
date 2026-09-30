# Micro

## Terminal interface

Start the runtime in one terminal; built-in tools need no separate executable:

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
  Both checklists expand with the terminal height. A `[more v]`, `[more ^]`,
  or `[more ^v]` label indicates hidden rows below, above, or in both directions;
  focus the list with Tab and use Up/Down to scroll.
  Changes are persisted and require an idle agent with an empty inbox, including
  no paused work. The name is fixed and session history is preserved; editing
  instructions updates the session's system prompt.
- Press `d` on the agent list to delete the selected agent. Choose whether to
  keep its saved conversation and private plugin files (the default) or erase
  them permanently, then press Enter to confirm or Esc to cancel.
  Queued and paused work is discarded; running agents cannot be deleted.
  Shared memory and other agents are untouched. If the default recipient is
  deleted, the first remaining agent becomes the default.
  Retained files stay on disk but are no longer listed in the TUI.
- Press `s` on an agent, its inbox, or its live session to compose a message, including optional
  conversation, reply, and steering-target IDs. A blank steering target starts
  an independent message; a target ID steers that unfinished run.
- Press `h` on an agent, its inbox, or its live session for durable message history.
  Use arrows to select, Enter to read, and `r` to refresh. Press `x` on a queued
  TUI-sent message to request retraction; Enter or `y` confirms and Esc cancels.
  Agent/runtime messages cannot be retracted by impersonating their sender.
  Retracted and cancelled entries remain in history.
- Press `v` on an agent or its inbox to open its live session: messages, tool
  calls, tool results, and completed responses. The view refreshes saved messages
  and agent status every second, with an animated working indicator while the
  agent is running. There is no token streaming or partial-response display.
  Scroll up to read history; End resumes following the latest activity.
  Esc returns to the agent list and stops polling, not the agent's work.
  Composing a message pauses polling; sending or cancelling returns to the live
  session and resumes updates. Failed sends keep your message draft for correction
  or retry. Refresh errors are shown and retried; paused agents show their error.
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

The default instructions encourage agents to try permitted tool creation or
delegation before declaring a missing capability. A child granted `create_tool`
is instructed to build reusable, parameterized tools, execute them, and return
the answer rather than stopping at tool creation. For example, a Seattle
time-and-activities request should use generic time/search tools with timezone
and query parameters, not city-specific tools. The prompt also requires honest
failure reporting, verification of real task results, and respect for Use/Assign
permissions and private tool ownership. These are model instructions, not new
permissions or a guarantee of model behavior. Existing sessions receive the
updated default prompt when their environment is loaded, preserving history
and agent-specific instructions.

`message` prints the accepted envelope ID, not the recipient's answer. The agent's
conversation and final output are saved in its session file. Each agent receives
`list_agents`, `agent_status`, `message`, `message_status`, and `retract_message`
tools automatically, including after reload.

The model-facing `message` tool accepts `to`, `content`, and optional `reply_to`
and `steer`.
The runtime supplies the sender identity and inherits the current conversation
ID. Agents send replies explicitly using the incoming sender and envelope ID;
final model output is not automatically sent back as another message.
The tool's receipt includes a `recipient` status snapshot and `delivery_status`
in addition to the accepted ID. `agent_status` accepts `{"name":"timekeeper"}` and reports `idle`,
`queued`, `running`, or `paused`, the pending count (including active work), the
active envelope ID when running, and any inbox error. These are agent processing
states, not proof that a delegated task has been completed. The TUI displays them
in agent lists and inbox views; refresh those views with `r`.

CLI messages also support `--sender`, `--conversation`, `--reply-to`, and `--steer`.

### Retraction and steering

Keep message receipt IDs. `message_status({"id":"..."})` reports a sender's
outgoing message as `queued`, `delivered`, `completed`, `retracted`, or `cancelled`.
`delivered` means committed to a run's session, not that a model has finished
reading or acting on it. `completed` means the run finished, not that its answer
is correct. History is stored with the environment and survives restart.

`retract_message({"id":"..."})` removes only the caller's own queued, undelivered
message. Delivery reservation and retraction are serialized: if the worker has
already claimed the message, retraction is too late. Repeating a successful
retraction is harmless. Retracting a queued original request also cancels its
queued steering messages. Delivered messages and effects cannot be undone.
Deleting an agent cancels its unfinished deliveries; message audit history
remains even when its session/plugin files are erased.

To change ongoing work, send a message with `steer` equal to the original
request's envelope ID and `to` equal to its recipient. Steering inherits the
target's conversation; an explicitly conflicting conversation is rejected.
It is not a new independent task, and `reply_to` alone does not make a message
steering. Unknown, finished, cancelled, and steering-message targets are rejected.

Steering is consumed before the next model request or between tool executions.
It does not interrupt a model request or a tool already executing. Unstarted
calls from the previous tool batch receive explicit "not executed" results so
the model can reconsider them while preserving valid tool-call/result history.
The run's original reply routing stays intact. Steering queued while a model
generates a final response is processed before the run is marked complete.
Paused runs retain steering but still require an operator retry.

There is no semantic deduplication: ordinary messages remain independent even
within one conversation. An agent that notices an accidental duplicate should
retract its receipt ID, not append another request telling the recipient to
ignore it. External adapters supply their own source/sender identity; only
internal model tools can act as an agent.

```sh
go run main.go environment message team researcher "Use the existing tool instead." --steer ORIGINAL_ENVELOPE_ID
go run main.go environment message-status team STEERING_ENVELOPE_ID
go run main.go environment retract-message team QUEUED_ENVELOPE_ID --sender cli
```

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

Built-in memory, messaging, agent creation, time, and tool creation run as native
Go handlers inside the runtime. Only agent-created tools execute as binary
plugins using the version-1 `describe`/`call` contract. There is no external
plugin directory, Go `.so` loading, or Python runner.

- [toolplugin/plugin.go](toolplugin/plugin.go) is the stdlib-only SDK and the
  authoritative version-1 wire contract: `Definition`, `Parameter`, `Manifest`,
  `Request`, `Response`, `Tool`, `Handler`, and `Serve`.
- [api/tools_builtin.go](api/tools_builtin.go) owns native built-in definitions
  and handlers. The runtime binds caller identity and conversation metadata,
  checks grants, and invokes these handlers directly. There is no subprocess
  or host callback socket for built-ins. Plugins receive no host socket.
- [api/plugins.go](api/plugins.go) contains the central subprocess launcher for
  plugin builds, discovery, and calls. Agent state,
  queues, memory, persistence, and authorization remain in the runtime.
- [api/plugin_catalog.go](api/plugin_catalog.go) validates private plugin
  manifests. [api/tool_catalog.go](api/tool_catalog.go)
  retains Use/Assign selection and caller binding. Tools are refreshed before
  each invocation and model call, without changing an agent's grants.

Existing built-in names and permission fields are unchanged. `DefaultTools()`
returns native definitions, including optional `create_tool`. Neither the runtime
nor direct Go callers need a bundled tool executable. Trusted custom Go
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
are tool failures. The optional `code` field retains its version-1 error
classification behavior.
Stdout is protocol-only; diagnostics belong on stderr. There is no streaming,
long-lived plugin process, or plugin-selected caller identity.

Names match `[a-z][a-z0-9_]{0,63}`. An agent-created plugin exposes exactly one tool with
nonempty descriptions (max 8192 bytes). Parameters use `string`, `integer`,
`number`, `boolean`, or `array` with primitive `items`. `enum` is supported for
strings. Nested object schemas are not supported in this version. Plugins cannot
declare `automatic: true` or replace a native built-in. Names must be unique
within the creating agent; different agents may create tools with the same name.

## Agent-created plugins

Enable Use for `create_tool` on a trusted agent. It accepts `name`, `source`
(a complete Go main package, max 64 KiB), `test_source` (a Go `package main` test
file, max 64 KiB), `test_arguments` (a JSON object encoded as a string), and
`expected_output` (the exact result string). It builds against the runtime's
embedded copy of the same SDK, requires a passing, non-skipped `TestToolSuccess`
via uncached `go test -json`, validates `describe`, and runs a successful
exact-match sample `call` before registering exactly one tool. The example above works
unchanged as `source`. Only the standard library and the SDK are available;
module downloads, CGO, workspace overrides, and automatic toolchain downloads
are disabled. A local Go 1.25+ toolchain is required for creation.

Factor the real tool handler into a testable function and exercise its
successful behavior in `TestToolSuccess`, with assertions on meaningful results.
For changing web responses, use deterministic `httptest` fixtures that drive
the real HTTP/parsing path; for clock logic, test a known timestamp. Error-path
tests may supplement this, but an invalid location or unavailable endpoint is
not success-path validation. Return production failures as errors, not
success-shaped `"ERROR: ..."` strings. The runtime rejects missing, skipped,
failing, or non-executed success tests; it cannot prove that arbitrary submitted
Go assertions faithfully test the intended semantics. A passing fixture test
also does not establish that a live service is available: call the registered
tool on the actual task before reporting success.

The creating agent can invoke its tool immediately, even in the same tool-call
batch. Source and binaries persist under the environment's `plugins` directory:

```text
data/<environment>/plugins/<agent>-<tool-name>-<unique-id>/
  bin/<tool-name>
  main.go
  main_test.go
  go.mod
  toolplugin/plugin.go
```

For example, `seattle_time` is the executable's filename, not the generic `tool`.
The agent prefix and unique ID prevent collisions between agents or retained
files from deleted agents. Saved agent state references the relative executable
path. Startup runs `describe`, not the sample call, to restore its schema. Missing or
invalid private binaries fail restoration. Tools remain private and cannot be
assigned to children. Editing an agent preserves them; unchecking Use for
`create_tool` disables creation, updates, and use of its private plugins. Re-enabling
it restores access. Individual deletion and sharing of private plugins are not
implemented. Failed tests, samples, and state saves do not
register a tool, though any effects of executing the sample cannot be undone.

Agents with `create_tool` also receive `update_tool` automatically. It takes the
same arguments and replaces only an existing private tool owned by that agent,
under the same name. Native tools and other agents' tools cannot be replaced.
The new version is built and validated in isolation, then the environment's
reference is switched atomically. Failed validation or persistence leaves the
current version intact; competing updates to the same version report a conflict.
Already-started calls keep their immutable executable. Prior version paths
remain recorded for ownership and are erased along with the current version
when deleting the agent with file erasure enabled.

Only the named layout above is supported; older directory-only references are
not migrated or loaded.
External plugin scanning, `serve --plugins`, `plugins reload`, and
`POST /tools/reload` are no longer supported. Remove the flag from launch commands;
the global Use/Assign catalogue contains only native tools. Saved agents with
old external-tool grants must have those grants removed before startup.

## Execution and sandboxing

**Plugins are not sandboxed yet.** Discovery, compilation, Go tests, sample calls, and
normal calls execute with the runtime user's OS access. Grant creation only to
trusted agents. A restricted environment and
temporary working directory are not security boundaries; plugins can access
files, networks, and processes, including secrets on disk.

Plugin calls use a fresh temporary working directory and receive only PATH and
LANG. Compilation also receives HOME and the Go build-cache path. Model API key
environment variables are not passed. Plugin requests are limited to 128 KiB;
stdout and stderr each to 1 MiB.
Discovery has a 10-second timeout, plugin calls 30 seconds, and compilation
90 seconds. Go tests use a 30-second test timeout and a 90-second command deadline,
also bounded by the enclosing operation. Cancellation kills the direct process; there are no memory, CPU,
or descendant-process guarantees.

Native handlers accept JSON objects up to 128 KiB and return strings up to 1 MiB.
They receive a 30-second context deadline, or 120 seconds for `create_tool` and `update_tool`,
and honor earlier caller cancellation. Native execution is cooperatively
cancelled; the runtime does not kill an in-process handler.

Nono is intentionally not integrated yet. The central launcher is the place to
wrap all execution stages with host-selected policies later, with no plugin
contract changes. Native tools continue to enforce their own permissions.

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
idempotency. Recorded explicit message IDs are not re-enqueued when repeated.
This does not identify equivalent text submitted with new IDs.

Use one process per environment data directory.

## Embedding in Go

Create or load an environment and its agents, then call `env.Start(ctx)` to
start workers. Submit `env.Message(ctx, api.Envelope{...})`, inspect with
`env.Inbox(name)` or `env.Status(name)`, and use `env.RetryInbox(ctx, name)` for paused work. Call
`env.Close()` to cancel workers, preserve unfinished messages, and wait for them
to exit.
Use `env.DeleteAgent(ctx, name, eraseFiles)` to remove a non-running agent and
discard its inbox, optionally erasing its saved conversation and private plugins.

The Unix-socket API exposes:

- `GET /tools`: native optional tool names and descriptions, excluding private plugins.
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
- `DELETE /environments/{id}/agents/{name}`: remove an agent and discard queued
  or paused work. Saved conversation/private plugin files are retained unless
  `?erase_files=true` is supplied. Shared memory and other agents are preserved.
  Deleting the default recipient promotes the first remaining agent, or clears
  the default if none remain. Returns 204 on success, 404 if absent, or 409 while
  running. A file-cleanup failure returns 500 with an explicit warning that the
  agent was deleted but some files remain; the deletion itself stays committed.
- `GET /environments/{id}/agents`: agent names, models, custom instructions,
  optional `tools`, `assignable_tools`, `allowed_models`, processing `status`,
  `active_envelope_id` when running, pending-message counts, and any inbox error.
- `GET /environments/{id}/agents/{name}/status`: name, processing status, pending
  count, active envelope ID when running, and any inbox error.
- `GET /environments/{id}/agents/{name}/session`: the latest committed session
  snapshot (`messages`), available even while the agent is processing work.
- `POST /environments/{id}/messages`: an `Envelope` JSON object using the Go field
  names (`Source`, `Sender`, `To`, `Content`, `ConversationID`, `ReplyTo`, optional
  `ID` and `Steer`); returns HTTP 202 with `id`, `status`, and `delivery_status`.
  Sources `agent` and `runtime` are reserved and rejected from external HTTP clients.
  Invalid/finished steering targets return 409, not a new independent task.
- `GET /environments/{id}/messages`: durable delivery history; optional `agent`
  query filters by recipient or agent sender.
- `GET /environments/{id}/messages/{message}`: one message's delivery state and envelope.
- `POST /environments/{id}/messages/{message}/retract`: JSON `{"Source":"cli","Sender":"cli"}`
  identifies the external sender. Returns the retracted delivery record, 403
  for wrong ownership, 404 if absent, or 409 if delivery has already started.
  Reserved agent/runtime sources are rejected.
- `GET /environments/{id}/agents/{name}/inbox`: pending `messages`, processing
  `status`, active envelope ID when running, and an optional `error` describing
  why the inbox is paused.
- `POST /environments/{id}/agents/{name}/inbox/retry`: resume a paused inbox.

The socket is owner-only. External adapters must authenticate their own callers;
the HTTP API reserves `Source: "agent"` for internal tool calls. Phone-messenger
and cron adapters themselves are not included.
# A2A

`web.Handler` and `web.Serve` expose every registered agent under `/api/agui/a2a/`
using A2A 1.0 JSON-RPC, implemented with `github.com/a2aproject/a2a-go/v2` v2.5.0.
Existing AG-UI endpoints remain available alongside A2A.

| Endpoint | Purpose |
| --- | --- |
| `GET /api/agui/a2a/` | Directory with `name`, `url`, and `agentCardUrl` for each agent |
| `GET /api/agui/a2a/{agent}/.well-known/agent-card.json` | Standard agent discovery card |
| `POST /api/agui/a2a/{agent}` | JSON-RPC requests; the trailing-slash form also works |

The underlying `agui.NewHandler` uses relative routes. Mount it with
`mux.Handle("/api/agui/", http.StripPrefix("/api/agui", agui.NewHandler(registry)))`
when serving without the embedded UI. For a reverse proxy, pass
`agui.WithA2ABaseURL("https://agents.example.com")`; include an external path
prefix in that URL when needed. Otherwise cards use the request's scheme and
Host. Forwarded headers do not override that origin.

## Sending messages

```sh
curl http://localhost:8080/api/agui/a2a/
curl http://localhost:8080/api/agui/a2a/Assistant/.well-known/agent-card.json
curl http://localhost:8080/api/agui/a2a/Assistant \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":"1","method":"SendMessage","params":{"message":{"messageId":"message-1","role":"ROLE_USER","parts":[{"text":"Explain what you can help with."}]}}}'
```

The result contains a task with its ID, context ID, status, and output artifacts.
Use the same `contextId` on subsequent messages to continue the conversation.
Each new message without a task ID creates a new A2A task. Use a new `messageId`
for every turn. To continue an input-required task, send its `taskId` instead.
A2A context IDs map to separate conversation threads for each agent, within the
host-resolved namespace; they do not alias arbitrary AG-UI thread IDs.

Supported methods include `SendMessage`, `SendStreamingMessage`, `GetTask`,
`ListTasks`, `CancelTask`, and `SubscribeToTask`. For streaming, use
`SendStreamingMessage` and `curl -N`; the response is SSE. Text deltas update one
artifact, followed by a final authoritative replacement and a completed status.
`SendMessage` blocks until completion or an input-required pause by default;
`configuration.returnImmediately: true` returns a task while execution continues.

```json
{"jsonrpc":"2.0","id":"2","method":"GetTask","params":{"id":"TASK_ID"}}
```

Replace the method with `CancelTask` to stop that task. Cancellation signals the
agent's stream broker, including work dispatched through a durable runtime.
Execution errors produce a failed task; malformed or unsupported input produces
an A2A error rather than a successful empty response. Disconnecting the HTTP
stream does not cancel the task; use `CancelTask` explicitly.

## Content and skills

The adapter accepts text and JSON data parts. Ordinary JSON data is passed as
user content. Agents configured with structured output return JSON data artifacts.
Binary/file input parts are rejected; file transport, push notifications, and
extended cards are not advertised by the default cards. Internal reasoning and
tool arguments are not emitted as user-visible text artifacts.

Skills are configured by the host. Message metadata and JSON data parts have no
special skill-selection or interrupt-resolution semantics. Request metadata is
available under `RunContext.A2A.metadata`; it cannot override namespace or
execution IDs.
The `Header` run-context field uses the same `Authorization` and `X-*` header
conventions as AG-UI.

## Tasks requiring input or authorization

Pauses use standard A2A task states and ordinary text status messages:

- `TASK_STATE_INPUT_REQUIRED` requests additional information. Elicitation
  messages and any requested JSON schema are described in the status text.
  Send an ordinary text or JSON data message on the same `taskId`.
- `TASK_STATE_AUTH_REQUIRED` requests approval or URL-based authorization.
  The status text explains the required action and includes any supplied URL.
  Approval without a URL must be handled through the host's authorization flow.
  When both authorization and information are pending, authorization takes priority.

The adapter never treats an A2A message as tool authorization. There is no
special approval JSON envelope, and sending "approve" does not grant approval.
Configure `agui.WithA2AAuthorizer(authorizer)` (or
`agents.WithA2AAuthorizer(authorizer)` when mounting manually). The host implements
`agents.A2AAuthorizer`:

- `Instructions` returns readable instructions, such as a link to its approval UI.
- `Authorize` waits for a trusted out-of-band decision and returns internal
  resolutions. It must honor context cancellation and authenticate and scope
  the decision to the pending operations, namespace, and caller.

The adapter keeps the A2A task active while awaiting authorization, then resumes
the same thread and publishes the result on the same task. Clients can poll or
subscribe; no follow-up message or private payload is needed. Canceling the task
cancels the authorization wait. Without an authorizer, a run requiring approval
fails with an explanatory status instead of leaving a task waiting indefinitely.
The adapter does not create an approval portal. The upstream SDK currently
rejects new messages while an authorization-waiting execution remains active;
use the host's out-of-band flow for approval or rejection, or `CancelTask` to stop.

Ordinary input is delivered to the configured agent runtime. Mapping an answer
to a pending tool form is the host/runtime's responsibility; the adapter does
not invent a standard form-submission schema or infer tool resolutions from
arbitrary text or JSON. A2A standardizes task continuation, not tool-specific
form or approval payloads.

The A2A context ID maps to the agent thread ID; the history layer resumes
from that thread without requiring a previous run ID or custom run-ID metadata.

## Namespace isolation and persistence

`agui.WithNamespaceResolver` applies to the directory, cards, and all JSON-RPC
operations. A resolver error returns HTTP 403. Without a resolver, the namespace
is `default`. Authentication and access to registered agents remain host-owned,
just as for AG-UI. Client `tenant` fields and metadata cannot select a different
namespace. Task ownership/listing follows the resolved namespace.

An adapter is retained per agent instance and namespace. Defaults use the A2A
SDK's in-memory task store and execution/event queues. This supports lookup,
listing and live subscriptions on one process, but A2A task records do not
survive a restart even when the agent uses a durable runtime. Completed tasks
are retrieved through `GetTask`; subscribing to a terminal task follows the
upstream SDK's terminal-task behavior.

Use `agui.WithA2AHandlerOptions(func(agentName, namespace string)
[]a2asrv.RequestHandlerOption { ... })` to supply A2A persistence, limits, or
`a2asrv.WithClusterMode(...)`. The factory is called once per adapter. Supplied
stores and queues must be isolated by both agent and namespace. Agent execution
continues to use its configured local, Temporal, or Restate runtime.

For manual mounting, `agent.A2A(card, agents.WithA2ANamespace(namespace),
agents.WithA2AHandlerOptions(...))` returns `InvokeHandler` and `AgentCardHandler`.
Retain that adapter across requests. The old unfinished adapter used the v0 SDK;
card imports now come from `github.com/a2aproject/a2a-go/v2/a2a`.

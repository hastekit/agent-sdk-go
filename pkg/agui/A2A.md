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

Message metadata can select skills using `"hastekit.skills":
{"enable":["review"],"disable":["writing"]}`. Required-skill rules still apply.
Supply the selection on each turn, including resumes. Request metadata is available
under `RunContext.A2A.metadata`; it cannot override namespace or execution IDs.
The `Header` run-context field uses the same `Authorization` and `X-*` header
conventions as AG-UI.

## Input-required tasks

A paused run becomes `TASK_STATE_INPUT_REQUIRED`. Its status message includes a
JSON data part with `type: "hastekit.interrupts"` and the run's `interrupts` array.
Reply on the same task with a data part like:

```json
{
  "messageId": "approval-1",
  "role": "ROLE_USER",
  "taskId": "TASK_ID",
  "parts": [{
    "data": {
      "type": "hastekit.interrupt_response",
      "resolutions": [{"call_id": "CALL_ID", "action": "approve"}]
    }
  }]
}
```

Use `reject` to decline, or include `content` on a resolution to submit a form
answer. The server obtains the previous run ID from stored task state.

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

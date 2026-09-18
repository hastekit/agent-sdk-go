# Workflows and agent tools

`LoadYAML` compiles a descriptor into the same `Compiled` graph used by the Go
builder. Version 1 supports `mcp`, `api`, `human`, `agent`, `if_else`, `switch`,
`delay`, and `javascript` nodes. YAML is trusted application configuration: API
nodes can make HTTP requests, and JavaScript executes in the host process.

## Build nodes in Go

All built-in nodes have public constructors returning `(workflow.Node, error)`.
The YAML loader calls these same constructors, so both paths share validation
and execution behavior.

| Constructor | Configuration |
|---|---|
| `NewMCPNode` | `MCPNodeConfig` |
| `NewAPINode` | `APINodeConfig` |
| `NewHumanNode` | `HumanNodeConfig` |
| `NewAgentNode` | `AgentNodeConfig` |
| `NewIfElseNode` | `IfElseNodeConfig` |
| `NewSwitchNode` | `SwitchNodeConfig` |
| `NewDelayNode` | `DelayNodeConfig` |
| `NewJavaScriptNode` | `JavaScriptNodeConfig` |

```go
review, err := workflow.NewHumanNode("review", workflow.HumanNodeConfig{
    Message: "${{ 'Approve payment of ' + input.amount + ' USD?' }}",
})
if err != nil { return err }

compiled, err := workflow.NewGraph("payment").
    AddNode("review", review).
    AddEdge(workflow.StartNode, "review").
    AddEdgeOnPort("review", "approved", workflow.EndNode).
    AddEdgeOnPort("review", "rejected", workflow.EndNode).
    Compile()
if err != nil { return err }
```

The constructor's node ID must match its `AddNode` key. Graph compilation checks
IDs and output ports. Built-ins can be mixed with your own `Node` implementations.
Expressions and outputs work as described below for YAML.

Pass an agent directly through `AgentNodeConfig.Agent`, and an MCP connector
through `MCPNodeConfig.Server`; Go callers do not need a named dependency map.
Timing fields use `time.Duration`, such as `DelayNodeConfig{Duration: time.Second}`.
JSON configuration values are copied at construction; injected agents, connectors,
and HTTP clients remain shared application dependencies.

The compiled graph can be executed, registered, or wrapped as an agent tool using
the APIs below. See the [runnable Go example](../../examples/workflow/programmatic/main.go)
(`go run ./examples/workflow/programmatic`).

## Load and execute

```go
compiled, err := workflow.LoadYAML(descriptorBytes, workflow.Dependencies{
    Agents: map[string]workflow.AgentRunner{"researcher": researcher},
    MCPServers: map[string]agents.MCPToolset{"github": githubConnector},
    HTTPClient: httpClient, // optional; configure transport/auth/network policy here
})
if err != nil { return err }

state, err := compiled.Execute(ctx, &workflow.Input{
    RunContext: map[string]any{
        "input": map[string]any{"query": "release notes"},
        "context": applicationRunContext, // optional; forwarded to agents/MCP credentials
    },
    Metadata: map[string]any{"namespace": "tenant-123"},
})
```

A missing run ID is generated once and retained in state. `Metadata` carries the
trusted `namespace`, `thread_id`, and `session_id`; namespace defaults to `default`
for direct executions. Each agent node gets an isolated child thread derived from
the workflow run ID and node ID.

## Register and invoke independently

Workflows can share the SDK registry with agents; neither an agent nor a model is
needed to invoke a registered workflow. Agent and workflow names are separate.

```go
registry := hastekit.NewRegistry()
if err := registry.RegisterWorkflow("review", compiled); err != nil { return err }

state, err := registry.RunWorkflow(ctx, "review", &workflow.Input{
    RunContext: map[string]any{"input": map[string]any{"amount": 150}},
})
// If state.Pause != nil, persist state, obtain a decision, call SetResume,
// then registry.RunWorkflow(ctx, "review", state) to continue.
```

`RegisterWorkflow` accepts optional `workflow.InvokeOption` values for runtime,
logger and step limits. Duplicate names return `hastekit.ErrWorkflowAlreadyRegistered`.
`Workflow(name)` and `WorkflowNames()` expose registered graphs. For applications
using only `pkg/workflow`, `workflow.NewRegistry()` provides `Register`, `Execute`,
`Workflow`, and `WorkflowNames` without the top-level SDK registry. The caller owns
execution state and must not execute the same `Input` concurrently.

## Execute nodes with Temporal

`NewTemporalExecutor` runs either a Go-built or YAML-compiled graph as a Temporal
workflow. Register it on a Temporal worker with the same graph and dependencies
on every worker polling that task queue:

```go
executor, err := workflow.NewTemporalExecutor("review", compiled,
    workflow.TemporalExecutorOptions{
        ActivityOptions: temporalworkflow.ActivityOptions{
            StartToCloseTimeout: 2 * time.Minute,
        },
        MaxSteps: 500,
    })
if err != nil { return err }

w := worker.New(temporalClient, "reviews", worker.Options{})
executor.Register(w)
if err := w.Start(); err != nil { return err }
defer w.Stop()

run, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
    ID: invocationID, TaskQueue: "reviews",
}, "review", &workflow.Input{
    RunContext: map[string]any{"input": map[string]any{"amount": 150}},
})
if err != nil { return err }
var state workflow.Input
if err := run.Get(ctx, &state); err != nil { return err }
```

Here `temporalworkflow` aliases `go.temporal.io/sdk/workflow`; `worker` and `client`
are the Temporal SDK packages. This entry point uses Temporal's workflow context,
so it is separate from the ordinary `WithRuntime` invocation option.

Nodes execute as activities, with parallel dispatch within each wave and stable
result merging. Node objects and their agent/MCP/HTTP dependencies stay on the
worker; inputs, outputs, and pause checkpoints must be serializable by Temporal.
Built-in delay nodes use durable Temporal timers and occupy no activity worker
while waiting. Conditional-edge Go routers run in the workflow itself and must
be deterministic and free of I/O. Keep the graph stable for existing histories;
use a new workflow type/version when changing its topology or behavior.

Defaults are a five-minute activity timeout, ten-second heartbeat timeout, and
one attempt. Supply `ActivityOptions.RetryPolicy` to enable retries, and make
external side effects idempotent before doing so. Activities heartbeat to receive
cancellation and must honor their context. A failing node cancels its wave's
other pending activities/timers. Cancelling a client `Get` context only stops
waiting: use `temporalClient.CancelWorkflow` to cancel the server-side execution.

A human pause completes this Temporal invocation with a checkpoint. Persist the
returned `Input`, call `SetResume`, and start another invocation with a new
Temporal workflow ID and the saved state; completed nodes are skipped. Alternatively,
a host Temporal workflow can call `executor.Execute(ctx, state)`, wait for a
signal when it pauses, set the decision, and call it again. Register the executor
on that worker in either case. No approval signal protocol is imposed by this API.

On failure, normal Temporal error semantics apply: `WorkflowRun.Get` returns the
error rather than partial state. A host workflow calling `executor.Execute`
directly can capture its returned partial `Input` before deciding how to report
the failure. Activity history remains available for diagnostics. This does not
make the separate HTTP handler's checkpoint store persistent.

See the [Temporal example](../../examples/workflow/temporal/main.go). Start
`temporal server start-dev`, then run `go run ./examples/workflow/temporal`.

## Independent HTTP invocation

```go
handler := hastekit.NewWorkflowHTTPHandler(registry, workflow.HTTPConfig{
    // Resolve the namespace from your application's authenticated request.
    NamespaceResolver: resolveNamespace,
    // Optional trusted context for MCP credential providers and nested agents.
    RunContextResolver: resolveApplicationRunContext,
})
// handler can be served directly or mounted on /workflows and /workflows/.
```

The handler provides these routes:

| Method | Path | Purpose |
|---|---|---|
| GET | `/workflows` | List registered workflow names |
| POST | `/workflows/{name}/runs` | Start a run with `{"input": {...}}` |
| GET | `/workflows/{name}/runs/{run_id}` | Read status, output or interrupts |
| POST | `/workflows/{name}/runs/{run_id}/resume` | Submit interrupt resolutions |

Start and resume execute synchronously until completion, pause or failure.
Disconnecting/cancelling the request cancels execution. Start returns HTTP 201,
resume returns 200, and both include `X-Workflow-Run-ID`. A response contains
`run_id`, `workflow`, `status` (`running`, `paused`, `completed`, or `failed`), and
`output` or `interrupts` as appropriate. Runtime failures return 500 and are logged
server-side. Unknown/foreign-namespace runs return 404; concurrent or completed
resumes return 409.

```sh
curl -X POST localhost:8080/workflows/review/runs \
  -H 'Content-Type: application/json' \
  -d '{"input":{"amount":150}}'

# Use run_id and interrupts[0].function_call_message.call_id from the response.
curl -X POST localhost:8080/workflows/review/runs/RUN_ID/resume \
  -H 'Content-Type: application/json' \
  -d '{"resolutions":[{"call_id":"INTERRUPT_ID","action":"approve","content":{"note":"Reviewed"}}]}'
```

Checkpoint state stays server-side: clients cannot set node statuses, continuation
state, namespace, or application run context. Requests are limited to 1 MiB by
default. The handler validates resolution IDs, actions and form content before
resuming; malformed decisions leave the run paused. A missing/empty namespace
resolver uses `default`; applications are responsible for authentication.

**HTTP runs are stored in memory on one handler instance.** Reuse that instance;
restarting it loses checkpoints. Runs expire one hour after the latest execution,
and the handler holds at most 1000 runs by default (`RunTTL`, `MaxRuns`). Running
executions are never evicted; when capacity is full, new starts return 503. This
API adds no cross-process persistence or crash-safe execution. For durable storage,
use the registry's Go API and persist the complete `Input` in your application.
The embedded chat UI is unchanged.

## Provide a workflow to an agent as a tool

```go
workflowTool, err := workflow.NewTool(
    "research", "Research a topic using the configured workflow",
    map[string]any{
        "type": "object",
        "properties": map[string]any{"query": map[string]any{"type": "string"}},
        "required": []string{"query"},
    },
    compiled,
)
if err != nil { return err }

agent, err := hastekit.NewAgent(&hastekit.AgentConfig{
    Name: "assistant",
    LLM: model,
    Tools: []agents.Tool{workflowTool},
})
```

The same wrapper accepts a graph built with `NewGraph(...).Compile()`. Optional
`InvokeOption` arguments select the runtime, logger and maximum steps. Tool
arguments become `RunContext.input`; the parent agent's run context is available
as `RunContext.context`. Namespace/session/thread identity comes from the calling
agent, not model-supplied arguments. The completed tool result contains workflow
state excluding the application `context` field.

When a node pauses, the tool returns an agent interrupt and saves a JSON checkpoint
through `StateUpdates`. The existing agent and AG-UI approval/form flow can resume
it; completed nodes are not re-executed. Checkpoints include private continuation
data and should be stored with the same access controls as conversation history.
Keep a workflow definition and its named dependencies stable while runs are paused.
This does not add crash-safe exactly-once execution: external effects made before
a checkpoint can repeat if the enclosing tool execution fails and is retried.

## Descriptor and expressions

```yaml
version: 1
id: lookup
nodes:
  - id: search
    type: mcp
    config:
      server: github
      tool: search_issues
      arguments:
        query: '${{ input.query }}'
  - id: summarize
    type: agent
    config:
      agent: researcher
      message: '${{ "Summarize: " + JSON.stringify(nodes.search) }}'
edges:
  - {from: START, to: search}
  - {from: search, to: summarize}
  - {from: summarize, to: END}
```

Each node has `id`, `type`, and `config`. Edges have `from`, `to`, and optional
`port` (defaults to `default`). `START` and `END` are reserved. Without START
edges, nodes without incoming edges are roots. Cycles, duplicate node IDs,
unknown dependencies, unknown fields and invalid output ports are rejected.
The existing walker executes in waves; it does not provide a barrier join for
branches of unequal length. Design joins accordingly.

Outputs live under `RunContext.nodes.<node-id>`. A whole YAML scalar of the form
`${{ expression }}` evaluates JavaScript and preserves the resulting JSON type.
Use JavaScript template literals to construct strings. Expressions can read
`input`, `nodes`, `context` (the whole RunContext), and `metadata`.
They evaluate against copies and cannot mutate shared workflow state.

## Node configuration

| Type | Configuration | Output ports |
|---|---|---|
| `mcp` | `server`, `tool`, optional `arguments` object | `default` |
| `api` | `url`, optional `method`, `headers`, JSON `body`, `timeout` | `default` |
| `human` | `message`, optional JSON `schema` for a form | `approved`, `rejected` |
| `agent` | `agent`, `message` | `default` |
| `if_else` | `condition`: JavaScript expression returning a boolean | `true`, `false` |
| `switch` | `value`, `cases`: ordered `{value, port}` entries | matching port or `default` |
| `delay` | `duration`: Go duration such as `500ms` or `2m` | `default` |
| `javascript` | `code`: synchronous function body with `return` | `default` |

`api` resolves URL, headers and body expressions. It fails on non-2xx responses,
limits response size to 4 MiB by default, and applies a 30-second request timeout.
Its output is `{status, headers, body}`; JSON bodies are decoded, others are text.
Use `Dependencies.MaxResponseBytes` and node `timeout` to adjust limits. An injected
HTTP client can impose additional limits, endpoint restrictions and authentication.

`mcp` matches the model-facing tool name from the named connector's tool list,
including any configured prefix. It honors tool approval requirements and saves
MCP elicitation continuations across pauses. Background-task tool results are not
supported. `agent` invokes `Run(ctx, ...)`, propagates child interrupts, and resumes
the same child thread with the saved previous run ID.

`human` pauses until a decision is supplied. For direct execution:

```go
if state.Pause != nil {
    // Persist the full state and await the user's answer before continuing.
    state.SetResume(state.Pause.NodeID, map[string]any{
        "action": "approve", // or "reject"
        "content": map[string]any{"note": "Looks good"},
    })
    state, err = compiled.Execute(ctx, state)
}
```

Form content is validated against the configured JSON schema. For directly resumed
MCP/agent nodes, the decision's `messages` field carries the ordinary agent
interrupt-resolution messages. The tool wrapper constructs these automatically.
Parallel pauses retain separate continuations and are presented one at a time.

```yaml
# Examples of the remaining node configurations:
nodes:
  - id: request
    type: api
    config:
      url: https://example.com/api/items
      method: POST
      body: {name: '${{ input.name }}'}
      timeout: 10s
  - id: decide
    type: if_else
    config: {condition: 'nodes.request.body.count > 0'}
  - id: route
    type: switch
    config:
      value: '${{ input.priority }}'
      cases:
        - {value: high, port: urgent}
        - {value: low, port: routine}
  - id: wait
    type: delay
    config: {duration: 2s}
  - id: transform
    type: javascript
    config:
      code: |
        return {names: nodes.request.body.items.map(item => item.name)};
```

JavaScript uses Goja with a fresh VM, a bounded call stack, context cancellation,
and a one-second execution timeout by default (`Dependencies.JavaScriptTimeout`).
No Node.js, filesystem, network, timer, or module bindings are installed. Async
results are rejected. This is not a process sandbox and does not impose a heap
limit; use trusted scripts. Delays are cancellable in-process timers, not durable
scheduled timers. The existing runtime interface remains available for custom
execution policies.

Run the self-contained approval example with `go run ./examples/workflow/yaml`.
It fetches a review limit from a local HTTP API, pauses for review when needed,
and calls a local MCP `record_approval` tool on the approved branch. The example
starts both demo servers automatically; no external services or credentials are needed.

To serve the same demo as an independent workflow API instead of auto-approving it:

```sh
go run ./examples/workflow/yaml -listen localhost:8080
```

The local API and MCP demo servers are still started automatically. The workflow
is registered as `review` and pauses until a resolution arrives over HTTP.

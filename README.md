# Golang Agent Harness SDK

[![Go Reference](https://pkg.go.dev/badge/github.com/hastekit/agent-sdk-go.svg)](https://pkg.go.dev/github.com/hastekit/agent-sdk-go)
[![Go Report Card](https://goreportcard.com/badge/github.com/hastekit/agent-sdk-go)](https://goreportcard.com/report/github.com/hastekit/agent-sdk-go)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

A powerful Golang SDK for building AI agents and making LLM calls across multiple providers with a unified API. Switch between OpenAI, Anthropic, Gemini, and more with just a single line change.

## Features

- **🔄 Multi-Provider Support** - Unified API for OpenAI, Anthropic, Gemini, and more
- **🧅 Gateway Middleware** - Compose retries, provider fallback, and your own around every LLM call
- **🤖 Agent SDK** - Build sophisticated AI agents with tools, memory, and multi-step reasoning
- **👤 Human-in-the-Loop** - Integrate human feedback and approval workflows
- **🛡️ Durable Execution** - Create fault-tolerant agents with Restate or Temporal
- **🔧 Tool Calling** - Function calling and MCP (Model Context Protocol) tool integration
- **🪝 Hooks** - Intercept tool calls and model calls for auth, budgets, and audit
- **🏷️ Tool Annotations** - MCP-style behavioural hints on both MCP and function tools
- **💾 Conversation History** - Maintain context across interactions with built-in persistence
- **🧩 Sub-Agents & Handoffs** - Call a specialist as a tool, or transfer the conversation to it
- **🎚️ Steering** - Send a correction into a run already in flight
- **🌊 Streaming Support** - Real-time streaming responses for better UX
- **🛑 Cancellation** - Stop in-flight runs cleanly, including mid-stream and mid-tool-call
- **📝 Structured Output** - JSON schema validation for reliable structured responses

## Table of Contents

- [Installation](#installation)
- [Quick Start](#quick-start)
- [Usage](#usage)
  - [LLM Client](#llm-client)
    - [Middleware](#middleware)
    - [Retries](#retries)
    - [Fallback](#fallback)
    - [Per-Model Middleware](#per-model-middleware)
  - [Agents](#agents)
    - [Sub-Agents](#sub-agents)
    - [Handoffs](#handoffs)
    - [Steering a Running Agent](#steering-a-running-agent)
  - [AG-UI](#ag-ui)
  - [Tools](#tools)
    - [Background Tool Execution](#background-tool-execution)
  - [Skills](#skills)
  - [Hooks](#hooks)
    - [Adding a Message to a Model Call](#adding-a-message-to-a-model-call)
    - [Hook State](#hook-state)
  - [Conversation History](#conversation-history)
  - [Durable Agents](#durable-agents)
- [Documentation](#documentation)
- [Examples](#examples)
- [License](#license)

## Installation

```bash
go get -u github.com/hastekit/agent-sdk-go
```

**Requirements:**
- Go 1.25.0 or higher

## Quick Start

### Simple Agent

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"

    hastekit "github.com/hastekit/agent-sdk-go"
    "github.com/hastekit/agent-sdk-go/pkg/agents"
    "github.com/hastekit/agent-sdk-go/pkg/agents/history"
    "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
    "github.com/hastekit/agent-sdk-go/pkg/utils"
)

func main() {
    // Configure an LLM client and bind a model.
    client := hastekit.NewLLMClient([]hastekit.ProviderConfig{
        {
            ProviderName: hastekit.ProviderOpenAI,
            ApiKeys: []*hastekit.APIKeyConfig{
                {Name: "default", APIKey: os.Getenv("OPENAI_API_KEY")},
            },
        },
    })

    // Create agent
    agent := hastekit.NewAgent(&hastekit.AgentConfig{
        Name:        "Assistant",
        Instruction: hastekit.NewPrompt("You are a helpful assistant."),
        LLM:         client.Model("OpenAI/gpt-4o-mini"),
        Parameters: responses.Parameters{
            Temperature: utils.Ptr(0.7),
        },
    })

    // Execute agent — returns a handle for streaming chunks + result.
    handle, err := agent.Execute(context.Background(), &agents.AgentInput{
        Message: history.Message{
            Messages: []responses.InputMessageUnion{
                responses.UserMessage("Hello! Tell me a joke."),
            },
        },
    })
    if err != nil {
        log.Fatal(err)
    }

    // Result() drains the chunk stream and returns the aggregated output.
    // For live streaming, range over handle.Chunks then call handle.Wait().
    out, err := handle.Result()
    if err != nil {
        log.Fatal(err)
    }

    fmt.Println(out.Output[0].OfOutputMessage.Content[0].OfOutputText.Text)
}
```

`agent.Execute` is non-blocking and returns an `*AgentHandle`:

```go
type AgentHandle struct {
    StreamID string                          // Broker channel id for this run
    Chunks   <-chan *responses.ResponseChunk // Live chunks; channel closes when run ends
}

func (h *AgentHandle) Stop(ctx context.Context) error    // graceful cancel at next iteration
func (h *AgentHandle) Wait() (*AgentOutput, error)       // pair with manual Chunks draining
func (h *AgentHandle) Result() (*AgentOutput, error)     // drain Chunks + return output
```

## Usage

### LLM Client

`hastekit.NewLLMClient` takes a list of provider configs and returns a client.
Bind a model with `client.Model("Provider/model")` — the returned value satisfies
the `llm.Provider` interface and exposes `NewResponses`,
`NewStreamingResponses`, and friends.

```go
// Single provider
client := hastekit.NewLLMClient([]hastekit.ProviderConfig{
    {
        ProviderName: hastekit.ProviderOpenAI,
        ApiKeys: []*hastekit.APIKeyConfig{
            {Name: "default", APIKey: os.Getenv("OPENAI_API_KEY")},
        },
    },
})

// Multiple providers — switch by changing the model string
client := hastekit.NewLLMClient([]hastekit.ProviderConfig{
    {
        ProviderName: hastekit.ProviderOpenAI,
        ApiKeys: []*hastekit.APIKeyConfig{
            {Name: "default", APIKey: os.Getenv("OPENAI_API_KEY")},
        },
    },
    {
        ProviderName: hastekit.ProviderAnthropic,
        ApiKeys: []*hastekit.APIKeyConfig{
            {Name: "default", APIKey: os.Getenv("ANTHROPIC_API_KEY")},
        },
    },
})

openai := client.Model("OpenAI/gpt-4o-mini")
claude := client.Model("Anthropic/claude-sonnet-4-5")
```

Provider constants: `hastekit.ProviderOpenAI`, `ProviderAnthropic`,
`ProviderGemini`, `ProviderXAI`, `ProviderBedrock`, `ProviderOllama`,
`ProviderOpenRouter`, `ProviderElevenLabs`, `ProviderSarvam`,
`ProviderDeepSeek`, `ProviderMoonshot` (Kimi models), `ProviderZAI` (GLM
models).

#### Middleware

Every call a client makes runs through a middleware chain. Nothing is
installed unless you ask for it — a call that fails is otherwise reported as
it happened — except tracing, which every client adds innermost so each
attempt gets its own span.

```go
import "github.com/hastekit/agent-sdk-go/pkg/gateway/middleware"

client := hastekit.NewLLMClient(configs, hastekit.WithMiddleware(
    middleware.NewFallbackModels("Anthropic/claude-sonnet-4-5"),
    middleware.NewRetry(middleware.RetryConfig{}),
))
```

The chain is written outermost first, and the order is yours to choose.
Fallback belongs outside retry: that way a provider is retried on its own
before the chain gives up on it, where the other way round a 503 that would
have cleared on the second attempt costs you a switch to a different model
instead.

Anything satisfying `gateway.Middleware` goes in the same list, so a budget
check, a cache, or a request log sits alongside the built-in ones:

```go
type auditLog struct{}

func (auditLog) HandleRequest(next gateway.RequestHandler) gateway.RequestHandler {
    return func(ctx context.Context, p llm.ProviderName, key string, r *llm.Request) (*llm.Response, error) {
        resp, err := next(ctx, p, key, r)
        record(p, r.GetRequestedModel(), err)
        return resp, err
    }
}

func (auditLog) HandleStreamingRequest(next gateway.StreamingRequestHandler) gateway.StreamingRequestHandler {
    return next
}

client := hastekit.NewLLMClient(configs, hastekit.WithMiddleware(auditLog{}, middleware.NewRetry(middleware.RetryConfig{})))
```

Middleware that needs the provider configuration — fallback, which resolves a
key for a provider the caller never named — is handed it when the chain is
installed, so you never pass a config store yourself.

#### Retries

`middleware.NewRetry` re-issues a call that failed for a reason another
attempt could plausibly fix. The zero `RetryConfig` is a usable policy: three
attempts, 500ms initial backoff doubling to a 30s ceiling, jittered. A
provider that names its own delay in a `Retry-After` header is obeyed as given
rather than jittered — it knows when its limit resets.

```go
middleware.NewRetry(middleware.RetryConfig{
    MaxAttempts:    5,               // counts the first call
    InitialBackoff: time.Second,
    MaxBackoff:     time.Minute,
})
```

Retried: 408, 409, 425, 429, and 500/502/503/504, plus transport failures —
timeouts, connection resets, truncated bodies. Not retried: a cancelled
context, and every 4xx that describes the request itself, since the same
request fails the same way on the next attempt.

Streaming is retried only up to the first chunk the caller sees. A stream that
fails before delivering anything is indistinguishable from one that never
opened, so it is retried transparently; once a chunk has been forwarded the
attempt is committed, because there is no way to un-send it and no provider
supports resuming a stream from the middle.

#### Fallback

`middleware.NewFallbackModels` sends a call to a different provider when the
one you asked for fails. Each target is tried in order, and — where retry sits
inside it — each target gets its own full retry budget before the chain moves
on.

```go
middleware.NewFallbackModels("Anthropic/claude-sonnet-4-5", "Gemini/gemini-2.5-flash")

// Or built explicitly, when you want to set the policy too.
middleware.NewFallback(middleware.FallbackConfig{
    Targets:      middleware.FallbackModels("Anthropic/claude-sonnet-4-5"),
    Fallbackable: func(err error) bool { return llm.StatusCodeOf(err) == 429 },
})
```

Each target's API key is resolved from the configs the client was built with,
so every target needs one; a target without a key is skipped rather than
failing the call. An id naming only a provider — `"OpenRouter"` — keeps the
model the caller asked for and only redirects the provider, which is what a
target on an OpenAI-compatible mirror wants.

Fallback moves on for almost any failure — a rate limit, an outage, an expired
key, a retired model. The exceptions are a cancelled context and the two
statuses that describe the request itself (400 and 422), since a request one
provider could not parse will not parse anywhere else.

> Streaming follows the same commit rule as retrying: a stream that fails
> before delivering a chunk can move to the next target, but one that has
> already delivered anything cannot — half an answer from one model finished
> by another is worse than a clean failure.

#### Per-Model Middleware

`WithMiddleware` on `Model` replaces the client's chain for that one model.
There is no merging: a chain is an ordered whole, and picking entries out of
one by type would not survive middleware you wrote yourself.

```go
client := hastekit.NewLLMClient(configs, hastekit.WithMiddleware(
    middleware.NewRetry(middleware.RetryConfig{}),
))

// Inherits the client's chain.
fast := client.Model("OpenAI/gpt-4o-mini")

// Tries harder, and falls back.
critical := client.Model("OpenAI/gpt-4o", hastekit.WithMiddleware(
    middleware.NewFallbackModels("Anthropic/claude-opus-4-5", "Gemini/gemini-2.5-pro"),
    middleware.NewRetry(middleware.RetryConfig{MaxAttempts: 5}),
))

// An LLM judge, held to exactly what the provider did on the first attempt.
judge := client.Model("OpenAI/gpt-4o", hastekit.WithoutMiddleware())
```

A model naming its own chain builds one, so bind a model once at setup rather
than per request.

### Agents

#### Agent with Custom Tools

`hastekit.NewTool` turns any `func(ctx, In) (Out, error)` into a tool. The input
JSON schema is derived from the argument struct, and arguments/results are
marshalled for you:

```go
type WeatherArgs struct {
    Location string `json:"location" jsonschema_description:"City name"`
}

type Weather struct {
    TempC     float64 `json:"temp_c"`
    Condition string  `json:"condition"`
}

func getWeather(ctx context.Context, args WeatherArgs) (Weather, error) {
    // Your logic here
    return Weather{TempC: 22.5, Condition: "Sunny"}, nil
}

weatherTool := hastekit.NewTool(getWeather,
    hastekit.WithName("get_weather"), // optional; defaults to the function name
    hastekit.WithDescription("Get current weather for a location"),
    hastekit.WithReadOnly(true), // optional behavioural hint
)

// Use the tool
agent := hastekit.NewAgent(&hastekit.AgentConfig{
    Name:        "Weather Assistant",
    Instruction: hastekit.NewPrompt("You help users check the weather."),
    LLM:         client.Model("OpenAI/gpt-4o-mini"),
    Tools:       []hastekit.Tool{weatherTool},
})
```

Tools that implement the `agents.Tool` interface directly also work and can be
mixed into the same `Tools` slice. The interface is two methods — `Execute`, and
`GetToolDescriptor() *agents.BaseTool` for the tool's schema, name and flags —
and embedding `agents.BaseTool` supplies the second one, so a hand-written tool
only defines `Execute`:

```go
type DeleteUserTool struct {
    *agents.BaseTool
}

func NewDeleteUserTool() *DeleteUserTool {
    return &DeleteUserTool{
        BaseTool: &agents.BaseTool{
            RequiresApproval: true,
            ToolUnion: responses.ToolUnion{
                OfFunction: &responses.FunctionTool{
                    Name:        "delete_user",
                    Description: utils.Ptr("Permanently deletes a user account"),
                    Parameters: map[string]any{
                        "type":       "object",
                        "properties": map[string]any{"user_id": map[string]any{"type": "string"}},
                        "required":   []string{"user_id"},
                    },
                },
            },
        },
    }
}

func (t *DeleteUserTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
    // Your logic here
}
```

#### Sub-Agents

An agent can be given to another agent as a tool, so a specialist is called the
same way a function is — the caller stays in charge and gets the sub-agent's
answer back as a tool result:

```go
import "github.com/hastekit/agent-sdk-go/pkg/agents/tools"

researcher := hastekit.NewAgent(&hastekit.AgentConfig{
    Name:        "Researcher",
    Instruction: hastekit.NewPrompt("You research topics thoroughly."),
    LLM:         model,
})

agent := hastekit.NewAgent(&hastekit.AgentConfig{
    Name:        "Assistant",
    Instruction: hastekit.NewPrompt("You are a helpful assistant."),
    LLM:         model,
    Tools: []agents.Tool{
        tools.NewAgentTool(
            "research",
            "Research a topic in depth",
            researcher,
            tools.SubAgentContextModeNone,
        ),
    },
})
```

The context mode decides who keeps track of the sub-agent's conversation. Under
`SubAgentContextModeNone` the calling model does: `thread_id` is one of the tool's
parameters, and the thread the sub-agent ran on comes back in the result for it to
pass in next time. Under `SubAgentContextModeIsolated` the tool does, holding the
thread in the call's own state, so the sub-agent remembers its earlier turns
without the model having to carry an id around.

A sub-agent that pauses for approval pauses the whole run, however deeply it is
nested, and resuming resumes it in place rather than starting it again.

#### Handoffs

A handoff transfers the conversation instead of borrowing an answer: the target
agent takes over the thread and replies to the user directly.

```go
agent := hastekit.NewAgent(&hastekit.AgentConfig{
    Name:        "Triage",
    Instruction: hastekit.NewPrompt("Route the user to the right specialist."),
    LLM:         model,
    Handoffs: []*agents.Handoff{
        agents.NewHandoff("Billing", "Questions about invoices and payments", billingAgent),
        agents.NewHandoff("Support", "Technical troubleshooting", supportAgent),
    },
})
```

The model picks a target by calling the generated `transfer_to_agent` tool.

By default the next turn starts at the root agent again. Set `StickyHandoff: true`
on the agent to keep the thread with whichever specialist last handled it, so a
user mid-conversation with Billing is not re-triaged on every message — a later
handoff moves the thread on, and a handoff back to the root unsticks it.

#### Streaming Chunks and Cancellation

`agent.Execute` returns a handle. Range over `handle.Chunks` to forward live deltas (UI, SSE, logs); call `handle.Stop(ctx)` to stop the run — it records a "Cancelled by user" assistant turn in history and emits `run.completed` cleanly.

`Stop` does not wait for an iteration boundary. It reaches work already in flight:

- **Mid-stream** — the model call is cut off where it is, rather than waited out. Text that had already streamed still reached the client, but the turn is recorded as cancelled rather than as a half-answer the model never finished.
- **Mid-tool-call** — a running tool has its context cancelled; a tool that ignores cancellation is abandoned after a grace period so the run still ends.

Either way the loop's invariant holds: every `function_call` in history is answered, so a stopped thread is still a valid thread to resume from.

```go
handle, err := agent.Execute(ctx, &agents.AgentInput{
    Message: history.Message{
        Messages: []responses.InputMessageUnion{
            responses.UserMessage("Walk me through how to set up Postgres replication."),
        },
    },
})
if err != nil { log.Fatal(err) }

// Cancel after 5 seconds — the agent finishes its current step and stops gracefully.
go func() {
    time.Sleep(5 * time.Second)
    _ = handle.Stop(context.Background())
}()

for chunk := range handle.Chunks {
    if chunk.OfOutputTextDelta != nil {
        fmt.Print(chunk.OfOutputTextDelta.Delta)
    }
}

out, err := handle.Wait()
```

Provider streams now report terminal failures through `ResponseChunk.OfError`. Agent handles return those failures as errors; a stream that ends without `response.completed` also fails instead of returning a partial answer. Custom providers must emit a completion event on success.

The OpenAI, Anthropic, and Gemini clients attach the caller's context to HTTP requests and accept an optional `ClientOptions.HTTPClient` for custom timeouts and transports. Cancelling the request context stops both the HTTP request and blocked stream sends.

The `StreamID` on the handle (also returned in the `X-Stream-Id` HTTP header when serving over HTTP) lets you re-subscribe to the same broker channel — useful for resuming a stream after a page refresh, or for stopping the run from a different process.

#### Steering a Running Agent

A run does not have to be left alone until it finishes. The same broker that
carries a stop can carry a message into a run already in flight — the loop drains
the queue at iteration boundaries, the same cadence at which it checks for a stop,
and folds what it finds into the conversation before the next model call:

```go
err := broker.EnqueueMessage(ctx, streamID, history.Message{
    Messages: []responses.InputMessageUnion{
        responses.UserMessage("Actually, focus on the last quarter only."),
    },
})
```

The agent finishes the tool calls already running, then continues with the new
instruction in context, so a correction lands without losing the work so far.

### AG-UI

Agents are served to the browser over the [AG-UI protocol](https://github.com/ag-ui-protocol/ag-ui) — the standard event-stream protocol that frontend agent frameworks (CopilotKit, raw `@ag-ui/client`, etc.) speak. The `pkg/agui` package translates the SDK's streaming chunks into canonical AG-UI events (text messages, reasoning, tool calls, steps, human-in-the-loop interrupts) over SSE:

```go
import "github.com/hastekit/agent-sdk-go/pkg/agui"

// Agents register into a package-global registry when created.
hastekit.NewAgent(&hastekit.AgentConfig{
    Name: "Assistant",
    // ...
})

// AgentRegistry exposes the registered agents to the AG-UI handler.
registry := &hastekit.AgentRegistry{}

// Exposes:
//   GET  /agents                                   → registered agent names
//   POST /agents/{agent}/run                       → AG-UI run endpoint (SSE)
//   GET  /agents/{agent}/threads/{thread}/stream   → rejoin the run in flight (SSE)
//   GET  /agents/{agent}/runs                      → long poll: runs across namespaces
//   GET  /agents/{agent}/threads                   → stored conversation threads, newest first
//   GET  /agents/{agent}/threads/{thread}/messages → thread history as AG-UI messages
http.ListenAndServe(":8080", agui.NewHandler(registry))

// Or mount a single agent's run endpoint on an existing mux:
// mux.Handle("POST /assistant/run", agui.AgentHandler(agent))
```

For a zero-setup browser chat UI, the `pkg/agui/web` package embeds a ready-made [CopilotKit](https://copilotkit.ai) chat into your binary with `go:embed` — no Node toolchain or separate frontend deploy needed to *run* it:

```go
import "github.com/hastekit/agent-sdk-go/pkg/agui/web"

// Serves the embedded CopilotKit chat UI at / and the AG-UI protocol
// endpoints under /api/agui/*.
if err := web.Serve(":8080", &hastekit.AgentRegistry{}); err != nil {
    log.Fatal(err)
}
```

The embedded UI lists registered agents, shows a sidebar of prior conversations (select one to resume it on the same thread), streams assistant text, reasoning, and tool calls live, and renders CopilotKit's `useInterrupt` approval cards inline when a run pauses for human-in-the-loop tool approval.

Conversation listing works when the agent's persistence adapter implements `history.ThreadLister` — the SDK's built-in in-memory and file adapters both do. For adapters that can't enumerate threads, the listing endpoint answers `501` and the UI hides the picker.

The CopilotKit UI is a Vite/React app under [`pkg/agui/web/ui`](pkg/agui/web/ui); its build output is committed to `pkg/agui/web/static`, so `go build` never needs Node. Rebuild only when changing the UI source (`cd pkg/agui/web/ui && pnpm install && pnpm build`). CopilotKit v2 can't be loaded from a public ESM CDN (its dependency graph breaks esm.sh/jsDelivr), so it's bundled. To keep the embedded weight down to ~1MB (from ~17MB), the build aliases out CopilotKit's heaviest optional dependencies — the markdown renderer's Shiki/Mermaid/Cytoscape stack (swapped for a lightweight `react-markdown` shim), KaTeX's math fonts, and the dev-console web-inspector — none of which the chat needs. An offline, framework-free fallback UI is embedded at `/basic.html`.

Options (shared by `agui.NewHandler`, `agui.AgentHandler`, and `web.Serve`):

```go
web.Serve(":8080", client,
    agui.WithNamespace("user-123"), // conversation namespace (default "default")
    agui.WithSenderID("alice"),     // sender attribution (default "user")
    agui.WithFullHistory(),         // forward the client's full message list
                                    // (only for agents without persistence)
    agui.WithKeepalive(10*time.Second), // SSE keep-alive interval (default 15s)
)
```

Human-in-the-loop: when a run pauses for tool approval, the stream emits a `CUSTOM` event named `on_interrupt` (CopilotKit's `useInterrupt` convention) followed by `RUN_FINISHED` with `result.status: "paused"`. The client resumes by POSTing decisions back on the same thread under `forwardedProps.command.resume.decisions[]` (`{toolCallId, approved}`).

### Tools

#### MCP Tools Integration

Connect to MCP servers for access to standardized tools:

```go
import "github.com/hastekit/agent-sdk-go/pkg/agents/mcpclient"

// Connect to MCP server
mcpClient, err := mcpclient.NewClient(
    context.Background(),
    "sample"
    "http://localhost:9001/sse",
    mcpclient.WithTransport("sse"), // or "streamable-http"
    mcpclient.WithHeaders(map[string]string{
        "Authorization": "Bearer token",
    }),
    mcpclient.WithToolFilter("list_users", "get_user"), // Optional: filter tools
)
if err != nil {
    log.Fatal(err)
}

// Create agent with MCP tools
agent := hastekit.NewAgent(&hastekit.AgentConfig{
    Name:        "MCP Agent",
    Instruction: hastekit.NewPrompt("You are a helpful assistant."),
    LLM:         model,
    McpServers:  []agents.MCPToolset{mcpClient},
})
```

#### MCP Servers over stdio

Many MCP servers ship as a command rather than a URL. `WithCommand` runs one as a
child process and speaks to it over stdin/stdout — there is nothing to deploy, and
the process is started on demand and reused across tool calls:

```go
mcpClient, err := mcpclient.NewClient(context.Background(), "filesystem", "", // no endpoint
    mcpclient.WithCommand("npx", "-y", "@modelcontextprotocol/server-filesystem", "/tmp"),
    mcpclient.WithEnv(map[string]string{
        "GITHUB_TOKEN": "{{github_token}}", // templated from the run context
    }),
)
```

`WithCommand` selects the stdio transport on its own. The environment is added to
the one the host process already has, so the command stays findable on `PATH`.

Everything else is transport-agnostic: `WithToolFilter`, `WithApprovalRequiredTools`,
`WithDeferredTools`, `WithToolPrefix`, and the schema cache all behave the same
whichever transport carries the server.

#### Namespacing Tools from Several Servers

Two servers that both publish a `search` would collide in the single list of names
the model chooses from. `WithToolPrefix` namespaces one server's tools in the name
the model sees, while calls are still made on the server under its own name:

```go
mcpclient.WithToolPrefix("fs__") // exposes "read_file" as "fs__read_file"
```

The prefix is used verbatim, separator included — pass `"fs__"`, not `"fs"`. Tool
filters, approval, and deferred lists are written against the server's own names,
so adding a prefix does not change them.

#### Tool Annotations

Tools can advertise what they do. The hints mirror [MCP's tool annotations](https://modelcontextprotocol.io/docs/concepts/tools), so hints read off an MCP server and hints declared on a local function tool are the same thing — one policy can read both:

```go
readTool := hastekit.NewTool(listUsers,
    hastekit.WithName("list_users"),
    hastekit.WithTitle("List users"), // human-readable, for UI
    hastekit.WithReadOnly(true),
)

writeTool := hastekit.NewTool(deleteUser,
    hastekit.WithName("delete_user"),
    hastekit.WithDestructive(true),
    hastekit.WithIdempotent(false),
    hastekit.WithOpenWorld(false),
)
```

MCP tools carry whatever their server declared; nothing extra is needed to pick them up. Read the hints back off the tool's descriptor:

```go
if tool.GetToolDescriptor().Annotations.IsDestructive() {
    // gate it — see Hooks below
}
```

A tool call hook is handed the same descriptor, which is where a policy usually reads them.

Every hint is a pointer, so "nothing was said" stays distinguishable from "false was said". Prefer the `Is*` helpers over reading fields directly: they are nil-safe and apply MCP's defaults, which are deliberately conservative — an unset `DestructiveHint` reads as destructive, an unset `ReadOnlyHint` as not read-only.

> Hints are self-reported: they describe intent, not enforcement. Never let a hint from an untrusted MCP server widen what a tool is allowed to do.

#### Background Tool Execution

A tool that starts work outlasting the call answers with a `TaskID`. The run
does not wait: the model reads the tool's immediate output, carries on, and the
result is delivered later as its own turn.

`hastekit.NewBackgroundTool` is `NewTool` for that kind of work — write the
long-running function and it handles the rest:

```go
indexTool := hastekit.NewBackgroundTool(
    func(ctx context.Context, in IndexArgs, progress hastekit.ProgressReporter) (IndexResult, error) {
        for i, doc := range in.Docs {
            progress.Report(ctx, hastekit.ToolProgress{
                Progress: float64(i + 1), Total: float64(len(in.Docs)), Message: doc.Name,
            })
            index(doc)
        }
        return IndexResult{Indexed: len(in.Docs)}, nil
    },
    hastekit.WithName("index_docs"),
    hastekit.WithDescription("Index documents. Takes a while."),
)
```

The model gets an immediate answer naming the task, the run carries on, and
whatever the function returns is delivered to the thread when it returns.
Progress is nil-safe, so report freely whether or not anyone is listening, and
`WithStartedMessage` replaces what the model is told at the moment the task
starts. Every ordinary tool option — `WithName`, `WithDestructive`,
`WithNeedsApproval` — works the same way it does on `NewTool`.

The return value is encoded like an ordinary tool's, **unless it already is a
tool output** — then it travels as it is. That is how a task answers with an
image or a file rather than a line of JSON:

```go
chartTool := hastekit.NewBackgroundTool(
    func(ctx context.Context, in ChartArgs, progress hastekit.ProgressReporter) (*responses.FunctionCallOutputMessage, error) {
        png := render(in)
        return &responses.FunctionCallOutputMessage{
            Output: responses.FunctionCallOutputContentUnion{
                OfList: responses.InputContent{
                    {OfInputText: &responses.InputTextContent{Text: "chart rendered"}},
                    {OfInputImage: &responses.InputImageContent{ImageURL: utils.Ptr(png)}},
                },
            },
        }, nil
    },
    hastekit.WithName("render_chart"),
)
```

The function runs **off the run's path, not inside it**: in this process a
goroutine of the agent's, and under Temporal or Restate whatever that runtime
keeps for the task. That is why it is handed its arguments rather than closing
over them — it may well run somewhere the call that started it never reached,
so anything it needs has to travel in the arguments.

For work that has to be *started* now and only watched later — a job queued
with another service — implement `agents.BackgroundTool` yourself, which splits
starting from waiting:

```go
func (t *indexTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
    job, err := t.client.StartIndexing(ctx)
    if err != nil {
        return nil, err
    }
    return &agents.ToolCallResponse{
        FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
            ID: params.ID, CallID: params.CallID,
            Output: responses.FunctionCallOutputContentUnion{
                OfString: utils.Ptr("Indexing started, job " + job.ID),
            },
        },
        TaskID: job.ID,
    }, nil
}

// AwaitTask blocks until the job is done. Poll it, subscribe to it, wait on a
// channel — whatever the service offers.
func (t *indexTool) AwaitTask(ctx context.Context, task agents.BackgroundTaskRef, progress agents.ProgressReporter) (agents.BackgroundResult, error) {
    for {
        job, err := t.client.Job(ctx, task.TaskID)
        if err != nil {
            return agents.BackgroundResult{}, err
        }
        progress.Report(ctx, agents.ToolProgress{
            Progress: job.Done, Total: job.Total, Message: job.Phase,
        })
        if job.Finished {
            return agents.BackgroundResult{Output: job.Summary}, nil
        }
        time.Sleep(5 * time.Second)
    }
}
```

Implementing `AwaitTask` is what makes a tool a background tool, and
`NewBackgroundTool` is one implementation of it. A `TaskID` from a tool without
it fails the run — the work has already started, and nothing would ever report
it. Anything the wait needs beyond the task id travels on
`ToolCallResponse.TaskPayload`, and comes back as `BackgroundTaskRef.Payload`.

**Where the result lands.** When the task finishes, the agent looks at the
thread:

- a run is still going — the result joins its queue and is folded in at the
  next iteration boundary, the same cadence as a steering message;
- the agent is idle — a new run starts with the result as its input, so the
  agent reports back without being asked.

After a handoff the task belongs to the agent the **run entered at**, not to
the specialist that started it: a specialist reached by handoff is running
inside someone else's run, so its own history is not that conversation and its
own broker is not that stream. The result therefore wakes the root agent, which
can route back into the specialist by sticky handoff.

Either way the result arrives as its own turn rather than as a second output
for the original call, which is not something a provider will accept: that call
was answered the moment the tool returned. `BackgroundResult.Output` is a
`*responses.FunctionCallOutputMessage` — the same shape a tool returns from
`Execute` — and its content blocks are carried into that turn intact, so an
image or a file survives the trip.

**Events on the run's stream.** Starting a task emits a
`background_task.started` chunk on the run that started it, carrying the tool
call it belongs to and the stream the task will publish progress on:

```json
{"type":"background_task.started","task_id":"job-41ff","call_id":"call_1",
 "tool_name":"index_docs","stream_id":"…"}
```

When the result lands, `background_task.completed` is published on the
thread's stream carrying the same identifiers — announced by the delivery
itself, which has the task, the call and the tool in hand. The agent loop is
not involved: what it receives is an ordinary user turn, because the call that
started the task was answered when the tool returned and a provider will not
accept a second output against it, so it would have to be told what it was
looking at.

On an idle thread the announcement is published in the moment between the
delivery claiming the channel and the woken run opening on it — so it arrives
*before* that run's first chunk. Readers hold back what precedes a run and emit
it once the run has opened, which keeps `RUN_STARTED` first as AG-UI requires.

Over AG-UI both arrive as CUSTOM events (`hastekit.background_task_started` /
`..._completed`).

**Progress.** The reporter handed to `AwaitTask` publishes the same
`tool.progress` chunks a tool can emit during `Execute`, keyed to the call that
started the task — so a client updates the row it already drew rather than
growing a new one.

Each task streams on a **channel of its own**, not the thread's. The thread's
channel belongs to whichever run holds it, and a run claiming it resets the
transcript — so a task publishing there would have its progress wiped by the
next turn, and what survived would be interleaved into another run's stream
keyed to a call that run never made. On its own channel, progress survives
whatever the thread does, replays to a client that subscribes late, and the
channel closes when the task ends.

The channel id is `hastekit.StreamIDForTask(namespace, threadID, taskID)`, and
is also recorded on the run's `BackgroundTasks` entry — so a UI can either
derive it or read it, and decide for itself whether to watch:

```go
taskStream := hastekit.StreamIDForTask("user-123", threadID, "job-41ff")
chunks, err := broker.Subscribe(ctx, taskStream)
```

Task ids are expected to be unique per task: two tasks sharing an id share a
channel.

**Waiting for tasks.** `agent.WaitForBackgroundTasks()` blocks until everything
in flight has been delivered. No run waits on it — that is the point — but a
process shutting down should.

**Under a durable runtime.** The wait has to outlive the call that started it,
which a goroutine cannot do once the activity or step it ran in has ended. Each
runtime supplies its own way of keeping one, so the tool interface is unchanged
and only the machinery behind it differs:

| Runtime | How the wait is kept | How an idle thread is woken |
|---|---|---|
| Local | a goroutine | `agent.Execute` |
| Temporal | a child workflow, `ParentClosePolicy: ABANDON`, running `AwaitTask` as a long activity | a child `_AgentWorkflow` |
| Restate | a one-way send to `BackgroundTaskService`, whose `Await` handler journals each step | a one-way `WorkflowSend` to `AgentWorkflow` |

The delivery decision itself — join a live run, or claim the thread and start
one — is `agents.DeliverBackgroundResult`, shared by all three. Getting it
wrong is the same mistake everywhere: joining a run that has ended strands the
result, and starting one that has not leaves two runs writing a single stream.

An agent with no runner at all fails a tool that returns a `TaskID`, with
`agents.ErrBackgroundUnsupported`, rather than starting work nothing will ever
report. Supply your own with `AgentOptions.BackgroundRunner` to teach another
runtime the trick.

> MCP tools cannot start background tasks yet — the protocol has no task
> concept for the client to carry.

### Skills

A skill is a folder of instructions the agent reads only when it needs them — a house style, a procedure, a checklist too long to keep in the system prompt every turn. Write one as a `SKILL.md` with YAML frontmatter, and put any supporting files beside it:

```
skills/
└── changelog/
    ├── SKILL.md
    └── references/
        └── style.md
```

```markdown
---
name: changelog
description: Write a release changelog entry. Use whenever the user asks for release notes.
---

Group the changes under `Added`, `Changed`, `Fixed`, and `Removed`...
The full house style is in `references/style.md`.
```

Point the agent at that folder:

```go
registry, err := hastekit.NewSkillRegistryFromDir("./skills")
if err != nil {
    log.Fatal(err)
}

agent := hastekit.NewAgent(&hastekit.AgentConfig{
    Name: "Release_Agent",
    Instruction: hastekit.NewPrompt(
        "You help maintain this project's releases.",
        prompts.WithResolver(prompts.DefaultResolvers()...), // ResolveSkills lists them
    ),
    Skills: registry,
    LLM:    model,
})
```

The agent lists the skills in its prompt and adds the tool that reads them to its own tools, so a prompt can never advertise a skill the model has no way to open. A prompt runs only the resolvers it is given, so one that leaves out `ResolveSkills` gets a model that never hears about them — see [Prompt resolvers](#prompt-resolvers) below.

The prompt carries only each skill's name and description. The model calls `read_skill` with a name to pull in the instructions, and `read_skill` with a `file` to pull in one of the bundled files — so a long skill costs context only on the turns it is actually used.

Pass several directories to draw from more than one library — a shared set plus this agent's own, say:

```go
registry, err := hastekit.NewSkillRegistryFromDir("./skills", "/etc/agent/skills")
```

Reading happens once, at construction. To pick up edits on disk, build a new registry.

#### Shipping skills inside the binary

Where the skills are part of the program rather than of its deployment, `go:embed` puts the whole tree in the binary — no folder to mount, copy, or keep in sync:

```go
//go:embed skills
var skillsFS embed.FS

registry, err := hastekit.NewSkillRegistry(skillsFS)
```

Embedding the parent folder is enough: a skill is found wherever a `SKILL.md` sits, so there is no `fs.Sub` to get right. `NewSkillRegistry` takes any `fs.FS`, so this is also the hook for skills that come from somewhere else entirely.

#### Rules

The name comes from the frontmatter, or from the folder when the frontmatter omits it. A folder holding a `SKILL.md` is one skill, and everything below it belongs to that skill — so a `SKILL.md` bundled as an example or a template stays a bundled file rather than becoming a second, half-formed skill.

Loading fails loudly on a skill with no description, on broken frontmatter, on a directory that isn't there, and on the same name defined twice. Skills decide how the agent behaves, so a bad one should stop startup rather than go quietly missing at runtime.

Only files a skill actually bundles are reachable through the tool: a path that tries to traverse out of the skill folder is refused, so one skill cannot read another or the rest of the filesystem the skills were read from.

Skills work the same under the Temporal and Restate runtimes: the durable agent registers and wraps the reader tool along with the rest, so a `read_skill` call is journaled like any other tool call and replays from the journal rather than re-reading the folder.

#### Skills from somewhere else

`AgentConfig.Skills` takes an `agents.SkillProvider` — a source that lists its skills, supplies the tool that reads them, and introduces them to the model:

```go
type SkillProvider interface {
    Skills() []agents.Skill
    SkillTool() agents.Tool // nil when the model already has a way to read them
    SkillHint() string      // the prompt's prose: what they are, how to read one
}
```

The agent asks the source for all three, which is what keeps the prompt and the tools in step. `SkillHint` is the whole of the section's prose and goes in verbatim — the resolver writes the `## Skills` heading and the catalogue, nothing else. Only the provider can write that hint honestly: a `SkillRegistry` names its own `read_skill` tool, while a host serving skills its own way names whatever the model actually has. Say nothing and the model gets the bare catalogue, which beats a prompt naming a tool the agent does not have.

A source that returns no tool is one the model can already reach. `agents.SkillList` lists such skills and adds nothing:

```go
Skills: agents.SkillList{{Name: "changelog", Description: "Write a release changelog entry."}},
```

`agents.SkillsWithHint` is the same, plus the prose — for a host that serves skill files through a tool of its own:

```go
Skills: agents.SkillsWithHint{
    List: agents.SkillList{{
        Name:         "changelog",
        Description:  "Write a release changelog entry.",
        FileLocation: "/skills/changelog/SKILL.md",
    }},
    Hint: "Skills are specialised instructions for particular kinds of work. " +
        "Read one with the `read_file` tool at the location listed below.",
},
```

#### Prompt resolvers

The system prompt is built by a chain of resolvers, each handed what the last produced along with the run's dependencies:

```go
type PromptResolverFn func(prompt string, deps *agents.Dependencies) (string, error)
```

A prompt starts with an empty chain and is used exactly as written — nothing appended, no templating. `prompts.DefaultResolvers()` is the standard set: `ResolveSkills`, `ResolveHandoffs`, `ResolveDeferredTools`, `ResolveTemplate` — the sections the agent contributes, then the `{{ placeholder }}` pass over the whole thing. Pass what you want, in the order you want; repeated calls accumulate:

```go
hastekit.NewPrompt("You help maintain this project's releases.",
    prompts.WithResolver(prompts.DefaultResolvers()...),
    prompts.WithResolver(func(prompt string, deps *agents.Dependencies) (string, error) {
        return prompt + "\n\n## House rules\n\nBe brief.", nil
    }),
)
```

### Hooks

A hook wraps what the agent does, so cross-cutting concerns — auth, budgets, quotas, audit, approval policy — live in one place instead of inside every tool. Hooks can observe, or answer in place of the real call.

`ToolCallHook` wraps every tool call; `ModelCallHook` wraps every call to the model. `hastekit.Hook` is both. Implement only the half you care about by embedding the no-op other half:

```go
// A budget check that has no interest in tools.
type credits struct {
    agents.NoopToolCallHook // supplies the tool-call half
}

func (c *credits) GetName() string { return "credits" }

func (c *credits) BeforeModelCall(ctx context.Context, call *agents.ModelCall) (agents.ModelCallHookResult, error) {
    if balanceFor(call.RunContext) <= 0 {
        // Answering is kinder than failing: the run ends with a message the
        // user can read rather than an error they cannot.
        return agents.HandleModelCall(
            agents.ModelCallText("You're out of credits — top up to continue."),
        ), nil
    }
    return agents.ContinueModelCall(), nil
}

func (c *credits) AfterModelCall(ctx context.Context, call *agents.ModelCall, res *agents.ModelCallResult) (agents.ModelCallHookResult, error) {
    recordSpend(call.RunContext, res.Usage) // res.Usage is this one call
    return agents.ContinueModelCall(), nil
}

agent := hastekit.NewAgent(&hastekit.AgentConfig{
    Name:  "Assistant",
    LLM:   client.Model("OpenAI/gpt-4o-mini"),
    Tools: []hastekit.Tool{weatherTool},
    Hooks: []agents.Hook{&credits{}},
})
```

The tool-call side is the same, with the tool the call is against handed over alongside it. Combined with annotations, a policy hook is a few lines:

```go
type policy struct {
    agents.NoopModelCallHook // model-call half; this hook only guards tools
}

func (p *policy) GetName() string { return "policy" }

func (p *policy) BeforeToolCall(ctx context.Context, tool *agents.BaseTool, call *agents.ToolCall) (agents.ToolCallHookResult, error) {
    if tool.Annotations.IsDestructive() && !allowed(call.RunContext, call.Name) {
        // Short-circuit: the tool never runs, and this stands in as its output.
        return agents.HandleToolCall(
            agents.ToolCallResult(call, "Denied by policy."),
        ), nil
    }
    return agents.ContinueToolCall(), nil
}

func (p *policy) AfterToolCall(ctx context.Context, tool *agents.BaseTool, call *agents.ToolCall, resp *agents.ToolCallResponse) (agents.ToolCallHookResult, error) {
    audit(call.Name, call.RunContext)
    return agents.ContinueToolCall(), nil
}
```

Notes:

- **`Handled` is explicit.** `ContinueToolCall()` passes the call along; `HandleToolCall(resp)` says the hook answered and the real call never happens. It's a flag rather than a nil check, because "I answered, and the answer is nothing to say" differs from "carry on without me".
- **Run context comes along.** `call.RunContext` is the per-run map you set on `AgentInput`, so per-tenant data (a JWT, an org id) is available without threading it through every tool.
- **The tool arrives as plain data.** `tool` is the same `*agents.BaseTool` its `GetToolDescriptor` returns — name, schema, annotations, meta — because the real tool may be a proxy for one running in another process. It is always non-nil.
- **`GetName()` must be unique per agent and stable across deploys.** Durable runtimes name each hook's journaled step after it, so a renamed hook is a new step on replay.
- **Hooks run as their own durable steps.** Under Restate or Temporal each hook call is journaled, so a check that talks to a billing service is not re-run on every replay.
- **A `BeforeModelCall` hook sees the shape of the call, not the prompt** — model, tenant, loop iteration, `ContextTokens`, and usage so far. That's what a budget check needs, and it keeps the conversation from crossing a durable boundary twice.

#### Adding a Message to a Model Call

A `BeforeModelCall` hook can put a message in front of the model for one call.
The note is appended after the conversation and is **never stored**, so it is
true of the call it rides on and does not accumulate in the transcript of
every call after it — the same treatment the loop gives its own
"you have one turn left" reminder.

```go
func (p *policy) BeforeModelCall(ctx context.Context, call *agents.ModelCall) (agents.ModelCallHookResult, error) {
    if call.ContextTokens < 100_000 {
        return agents.ContinueModelCall(), nil
    }
    return agents.ContinueModelCall().WithMessages(
        responses.UserMessage("You are close to the context limit. Summarise findings before continuing."),
    ), nil
}
```

Hooks append in order, after the conversation. Only `BeforeModelCall` can add
one — by `AfterModelCall` there is no request left to add to — and nothing is
appended when a hook answers for the model, since the provider is never
called.

This is for a note the model should act on *now*. Reshaping the history itself
— trimming it, summarising it — belongs to the conversation summarizer, which
already owns the whole transcript; a hook is deliberately never handed it.

#### Hook State

`ModelCall.State` is the run's key-value scratchpad, the same one tools read
through `ToolCall.State`. Write to it by returning updates, exactly as a tool
returns `StateUpdates`:

```go
func (p *policy) BeforeModelCall(ctx context.Context, call *agents.ModelCall) (agents.ModelCallHookResult, error) {
    if call.State["warned"] == "1" {
        return agents.ContinueModelCall(), nil
    }
    return agents.ContinueModelCall().
        WithMessages(responses.UserMessage("Heads up: this run is nearly out of budget.")).
        WithStateUpdates(map[string]string{"warned": "1"}), nil
}
```

State persists with the thread, so a hook can remember something across
invocations — that it has already warned, so it warns once rather than every
iteration. Tools and hooks share one flat namespace: what a tool wrote, the
next call's hooks read. A write lands as soon as it is returned, so a later
hook in the chain reads what an earlier one just wrote; the last writer of a
key wins, and a hook that writes only its own keys never disturbs another's.

> `WithStateUpdates` is the only way to write. Assigning into `call.State`
> changes nothing: a durable runtime rebuilds that map from a serialized
> payload, so a write on the far side reaches nothing — and it is copied
> locally too, so the mistake fails the same way in both places rather than
> only once you deploy.

### Conversation History

Enable conversation memory across interactions:

```go
// Create a file-backed conversation manager
memory := hastekit.NewFileHistory("./conversations")

agent := hastekit.NewAgent(&hastekit.AgentConfig{
    Name:        "Memory Agent",
    Instruction: hastekit.NewPrompt("You are a helpful assistant."),
    LLM:         model,
    History:     memory, // Enable history
})

threadID := uuid.NewString()

// First interaction
handle, err := agent.Execute(context.Background(), &agents.AgentInput{
    Namespace: "user-123", // Bucket conversations by namespace
    ThreadID:  threadID,
    Message: history.Message{
        Messages: []responses.InputMessageUnion{
            responses.UserMessage("My name is Alice."),
        },
    },
})
out, err := handle.Result()

// Continue conversation — pass the same ThreadID to keep context.
handle, err = agent.Execute(context.Background(), &agents.AgentInput{
    Namespace: "user-123",
    ThreadID:  threadID,
    Message: history.Message{
        Messages: []responses.InputMessageUnion{
            responses.UserMessage("What's my name?"),
        },
    },
})
out, err = handle.Result()
```

The built-in memory and file history adapters scope thread IDs, run IDs, and summaries by namespace. The same IDs can coexist in different namespaces. Reads use the exact namespace, including the empty namespace; only `ListThreads` treats an empty namespace as a request to list all namespaces.

Passing `ThreadID` alone continues from the thread's tip. To branch from a specific earlier turn instead — a retry, or an edit of an earlier message — set `PreviousRunID` to the `RunID` of the run you want to continue from:

```go
out, _ := handle.Result()

handle, err = agent.Execute(ctx, &agents.AgentInput{
    Namespace:     "user-123",
    ThreadID:      threadID,
    PreviousRunID: out.RunID, // continue from this run, not the thread tip
    Message:       history.Message{ /* ... */ },
})
```

#### Reading a thread back

To render a thread — a chat window, an audit view — use `LoadTranscript`:

```go
transcript, err := memory.LoadTranscript(ctx, "user-123", threadID)
```

It returns the thread as written. That is deliberately not what the agent reads for itself: the agent's own history load is summary-aware, replacing the turns a summary covers with the summary, which is what keeps a long thread inside the context window. Right for the model, wrong for a UI — asking the summary-aware path for a whole summarized thread is exactly when the substitution kicks in, and the window loses its own early turns.

Adapters implement `history.TranscriptReader` to support this; the built-in in-memory and file adapters both do.

### Durable Agents

Create fault-tolerant agents that survive crashes and failures:

A durable agent is a regular agent with a durable `Runtime` attached via
`hastekit.WithRuntime`. Create the runtime, build the agent, then start the
runtime; invoke agents over HTTP with `hastekit.NewHTTPHandler()`.

#### Using Restate

```go
client := hastekit.NewLLMClient([]hastekit.ProviderConfig{
    {
        ProviderName: hastekit.ProviderOpenAI,
        ApiKeys: []*hastekit.APIKeyConfig{
            {Name: "default", APIKey: os.Getenv("OPENAI_API_KEY")},
        },
    },
})

// Restate service bind address + Redis for streaming
rt, err := hastekit.NewRestateRuntime("0.0.0.0:9081", "localhost:6379")
if err != nil {
    log.Fatal(err)
}
broker, err := hastekit.NewRedisStreamBroker("localhost:6379")
if err != nil {
    log.Fatal(err)
}

// Create durable agent
agent := hastekit.NewAgent(&hastekit.AgentConfig{
    Name:        "DurableAgent",
    Instruction: hastekit.NewPrompt("You are a helpful assistant."),
    LLM:         client.Model("OpenAI/gpt-4o-mini"),
    History:     hastekit.NewFileHistory("./conversations"),
}, hastekit.WithRuntime(rt, broker))

// Start Restate service, then serve the invoke endpoint
rt.Start()
http.ListenAndServe(":8070", hastekit.NewHTTPHandler())

// Register deployment with Restate server
// restate deployments register http://localhost:9081
```

#### Using Temporal

```go
client := hastekit.NewLLMClient([]hastekit.ProviderConfig{
    {
        ProviderName: hastekit.ProviderOpenAI,
        ApiKeys: []*hastekit.APIKeyConfig{
            {Name: "default", APIKey: os.Getenv("OPENAI_API_KEY")},
        },
    },
})

// Temporal server endpoint + Redis for streaming
rt, err := hastekit.NewTemporalRuntime("localhost:7233", "localhost:6379")
if err != nil {
    log.Fatal(err)
}
broker, err := hastekit.NewRedisStreamBroker("localhost:6379")
if err != nil {
    log.Fatal(err)
}

// Create Temporal agent
agent := hastekit.NewAgent(&hastekit.AgentConfig{
    Name:        "TemporalAgent",
    Instruction: hastekit.NewPrompt("You are a helpful assistant."),
    LLM:         client.Model("OpenAI/gpt-4o-mini"),
}, hastekit.WithRuntime(rt, broker))

// Start the Temporal worker, then serve the invoke endpoint
rt.Start()
http.ListenAndServe(":8070", hastekit.NewHTTPHandler())
```

## Documentation

- **[Full Documentation](https://docs.hastekit.ai/hastekit-sdk/introduction)** - Comprehensive guides and API reference
- **[Getting Started](https://docs.hastekit.ai/hastekit-sdk/setting-up)** - Setup and first steps
- **[Agent Guide](https://docs.hastekit.ai/hastekit-sdk/agents/simple-agent)** - Build AI agents
- **[Tool Integration](https://docs.hastekit.ai/hastekit-sdk/agents/tools/function-tools)** - Custom tools and MCP
- **[Durable Execution](https://docs.hastekit.ai/hastekit-sdk/agents/durable/restate)** - Fault-tolerant agents
- **[API Reference](https://pkg.go.dev/github.com/hastekit/agent-sdk-go)** - Go package documentation

## Examples

Explore complete working examples in the [documentation repository](https://github.com/hastekit/hastekit-docs/tree/master/examples):

### Agents
- [Simple Agent](https://github.com/hastekit/hastekit-docs/tree/master/examples/agents/1_simple_agent)
- [Tool Calling Agent](https://github.com/hastekit/hastekit-docs/tree/master/examples/agents/2_tool_calling_agent)
- [Multi-Turn Conversation](https://github.com/hastekit/hastekit-docs/tree/master/examples/agents/3_agent_multi_turn_conversation)
- [MCP Tools](https://github.com/hastekit/hastekit-docs/tree/master/examples/agents/4_agent_with_mcp_tools)
- [Agent as a Tool](https://github.com/hastekit/hastekit-docs/tree/master/examples/agents/5_agent_as_a_tool)
- [Human-in-the-Loop](https://github.com/hastekit/hastekit-docs/tree/master/examples/agents/6_human_in_the_loop)
- [Serving Agents](https://github.com/hastekit/hastekit-docs/tree/master/examples/agents/7_serving_agents)
- [Restate Agent](https://github.com/hastekit/hastekit-docs/tree/master/examples/agents/8_restate_agent)
- [Temporal Agent](https://github.com/hastekit/hastekit-docs/tree/master/examples/agents/9_temporal_agent)
- [Agent with Sandbox](https://github.com/hastekit/hastekit-docs/tree/master/examples/agents/10_agent_with_sandbox)
- [Sandbox Bash Tool](https://github.com/hastekit/hastekit-docs/tree/master/examples/agents/11_agent_with_sandbox_bash_tool)
- [Agent Handoff](https://github.com/hastekit/hastekit-docs/tree/master/examples/agents/12_agent_handoff)
- [MCP Tools over stdio](https://github.com/hastekit/hastekit-docs/tree/master/examples/agents/14_agent_with_stdio_mcp_tools)

## Supported Providers

| Provider | Text | Streaming | Tool Calling | Vision | Embeddings | Image Gen | Image Edit | Speech | Transcription |
|----------|:----:|:---------:|:------------:|:------:|:----------:|:---------:|:----------:|:------:|:-------------:|
| **OpenAI** | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Anthropic** | ✅ | ✅ | ✅ | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ |
| **Gemini** | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **xAI** | ✅ | ✅ | ✅ | ✅ | ❌ | ✅ | ✅ | ✅¹ | ❌ |
| **Bedrock** | ✅ | ✅ | ✅ | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ |
| **ElevenLabs** | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ✅ | ✅ |
| **Sarvam** | ✅² | ✅² | ✅² | ✅² | ❌ | ❌ | ❌ | ✅³ | ✅ |
| **DeepSeek** | ✅² | ✅² | ✅² | ✅² | ❌ | ❌ | ❌ | ❌ | ❌ |
| **Moonshot** (Kimi) | ✅² | ✅² | ✅² | ✅² | ❌ | ❌ | ❌ | ❌ | ❌ |
| **Z.ai** (GLM) | ✅² | ✅² | ✅² | ✅² | ❌ | ❌ | ❌ | ❌ | ❌ |

¹ Non-streaming only — xAI has no `NewStreamingSpeech`.
² Served by the chat-completions bridge (see below); vision depends on the model.
³ `NewStreamingSpeech` synthesizes in one call and emits a single audio delta — Sarvam's incremental TTS is a WebSocket API, not an HTTP stream.

**Text** is the Responses API (`NewResponses`), which is what agents use. OpenAI additionally implements the older Chat Completions API (`NewChatCompletion` / `NewStreamingChatCompletion`); so do the bridged providers below.

Sarvam, DeepSeek, Moonshot and Z.ai speak the OpenAI `/chat/completions` format but have no `/responses` endpoint. They are served by `providers/openaicompat`, a generic translation in both directions: a native Responses request becomes a chat completion (instructions → system message, parallel function calls collapsed onto one assistant message, tool results → `tool` messages, `text.format` → `response_format`), and the reply — including a streamed one — is reassembled into Responses output items and events. `reasoning_content` becomes a native reasoning item. Server-side tools (web search, image generation, code interpreter) have no equivalent and are dropped from the request. Default base URLs: Sarvam `https://api.sarvam.ai`, DeepSeek `https://api.deepseek.com`, Moonshot `https://api.moonshot.ai/v1`, Z.ai `https://api.z.ai/api/paas/v4` — each overridable through `BaseURL` on the provider config (Moonshot's mainland endpoint, Z.ai's Coding Plan or Zhipu endpoints, a self-hosted proxy).

Any other OpenAI-compatible endpoint can be added the same way: point `openaicompat.NewClient` at its base URL.

`ProviderOllama` and `ProviderOpenRouter` are not in the table because their capabilities aren't the SDK's to state: both are served by the OpenAI client, so every method is wired up and each call is passed straight through. What actually answers depends on the endpoint and the model behind it. OpenRouter defaults to `https://openrouter.ai/api/v1`; Ollama needs an explicit `BaseURL` on its provider config.

> A ❌ is not a graceful "unsupported" error. Unimplemented methods fall through to the embedded base provider, where the text and embedding methods `panic` and the media methods return `(nil, nil)` — so a call for a capability a provider doesn't have will either crash or hand back a silent nil. Check this table before reaching for a non-text method on a non-OpenAI provider.

## Architecture

```
agent-sdk-go/
└── pkg/
    ├── agents/              # Agent orchestration, hooks, tool annotations
    │   ├── runtime/         # Durable execution runtimes
    │   │   ├── restate_runtime/
    │   │   └── temporal_runtime/
    │   ├── agentstate/      # Run status and state
    │   ├── history/         # Conversation management
    │   ├── mcpclient/       # MCP tool integration
    │   ├── prompts/         # Prompt construction
    │   ├── sandbox/         # Sandboxed execution
    │   ├── streambroker/    # Stream brokers (memory, Redis)
    │   └── tools/           # Built-in tools
    ├── agui/                # AG-UI protocol + embedded chat UI
    ├── gateway/             # LLM gateway
    │   ├── llm/             # LLM request/response types
    │   └── providers/       # Provider implementations
    │       ├── openai/      # anthropic, gemini, xai, bedrock,
    │       ├── openaicompat/# chat-completions <-> responses bridge
    │       └── sarvam/      # deepseek, moonshot, zai, elevenlabs, ...
    ├── hastekitgateway/     # HasteKit Gateway adapters
    ├── knowledge/           # Knowledge / retrieval
    ├── telemetry/           # Tracing and metrics
    └── utils/               # Utilities
```

Runnable examples live in the [documentation repository](https://github.com/hastekit/hastekit-docs/tree/master/examples) — see [Examples](#examples) above.

## Contributing

We welcome contributions! Please see our [contributing guidelines](CONTRIBUTING.md) for details.

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

## Support

- **Documentation**: [docs.hastekit.ai](https://docs.hastekit.ai)
- **Issues**: [GitHub Issues](https://github.com/hastekit/agent-sdk-go/issues)
- **Discussions**: [GitHub Discussions](https://github.com/hastekit/agent-sdk-go/discussions)

## Related Projects

- [HasteKit Gateway](https://github.com/hastekit/hastekit-ai-gateway) - LLM Gateway with observability and agent builder
- [HasteKit Docs](https://github.com/hastekit/hastekit-docs) - Documentation and examples

---

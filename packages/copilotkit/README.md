# @hastekit/copilotkit

Use [CopilotKit](https://copilotkit.ai) as the UI for agents built with the
[HasteKit SDK](https://github.com/hastekit/agent-sdk-go). A HasteKit server
speaks [AG-UI](https://docs.ag-ui.com) itself, so no CopilotKit runtime is
needed: register a `HastekitAgent` with CopilotKit and it talks to the server
directly.

`HastekitAgent` is an AG-UI `HttpAgent` that knows the HasteKit server:

- **Stop** ends the run on the server, and the chat shows it winding down,
  instead of only closing the connection while the run carries on.
- **Reconnecting runs.** A dropped connection rejoins the run where it left
  off, a reload rejoins a run still going, and runs started elsewhere on the
  same thread can be joined.
- **Only the new turn is sent.** The server keeps the thread, so the request
  body does not grow with the conversation.
- **Interrupts**: tool approvals and MCP form and URL elicitations, answered
  through AG-UI 1.0 resume entries, including after a reload.
- **Client tools** registered with `useFrontendTool` run as soon as their
  call streams in, whichever way the server is configured to take results.
- **Steering**: send a message into a run that is still going.
- **Skills and MCP servers** the user has turned off are sent with every run.

## Install

```bash
npm install @hastekit/copilotkit @copilotkit/react-core @ag-ui/client @ag-ui/core rxjs
```

Requires `@copilotkit/react-core` 1.73 or later and `@ag-ui/client` 0.0.59 or later.

## Quick start

On the server, mount the AG-UI handler (see the SDK's AG-UI docs), for example
with `web.Serve`, which serves it under `/api/agui`.

In the app:

```tsx
import { CopilotChat, CopilotKitProvider } from "@copilotkit/react-core/v2";
import "@copilotkit/react-core/v2/styles.css";
import { HastekitAgent } from "@hastekit/copilotkit";
import { useMemo } from "react";

export function Chat({ threadId }: { threadId: string }) {
  const agent = useMemo(
    () => new HastekitAgent({ agentName: "support", threadId, baseUrl: "/api/agui" }),
    [threadId],
  );
  return (
    <CopilotKitProvider selfManagedAgents={{ support: agent }}>
      <CopilotChat agentId="support" threadId={threadId} />
    </CopilotKitProvider>
  );
}
```

`agentName` is the agent's name on the server; it is also the default
`agentId`. Pass `headers` for authentication and `fetch` to customise requests.
They are used for every request the agent makes.

By default, requests contain only new messages. Messages already submitted or
loaded from the server stay visible but are excluded from later requests, even
if their run failed before producing an assistant reply. Set `fullHistory: true`
only for a stateless server that needs the entire transcript on every request.

## Opening an existing thread

Load the thread's latest page before creating the agent and pass it as
`initialMessages`. CopilotKit clears the agent when it connects to a thread;
the agent restores these messages when that happens.

```ts
import { HastekitAgent, fetchThreadMessages } from "@hastekit/copilotkit";

const page = await fetchThreadMessages({ agentName: "support" }, threadId);
const agent = new HastekitAgent({ agentName: "support", threadId, initialMessages: page.messages });

// Later, for older messages:
const older = await fetchThreadMessages(agent.connection, threadId, { cursor: page.nextCursor });
agent.prependMessages(older.messages);
```

`page.run` says what the thread's last run left outstanding: interrupts waiting
on the user, and background tasks still working. `page.context` says how full
the context window was when that run ended (`ContextUsage`: tokens, and the
agent's window when the server knows it); runs report it
live after each model call as `HastekitEvent.ContextUsage`.

## Interrupts

A run that needs the user ends with an AG-UI interrupt outcome. Each interrupt
is one of three kinds:

| `kind`     | Asks the user to                              | Answer                                   |
| ---------- | --------------------------------------------- | ---------------------------------------- |
| `approval` | allow or reject a tool call                   | `{ id, approved }`                       |
| `form`     | fill in fields described by `schema`          | `{ id, approved: true, values }`, or `approved: false` to cancel |
| `url`      | visit `url`, then continue                    | `{ id, approved }`                       |

`useHastekitInterrupt` wraps CopilotKit's `useInterrupt`. It reads each
interrupt into a `HastekitInterrupt` and turns your decisions into the resume
entries the server expects:

```tsx
import { useHastekitInterrupt } from "@hastekit/copilotkit/react";

function Approvals() {
  useHastekitInterrupt({
    render: ({ interrupts, respond }) => (
      <div>
        {interrupts.map((it) => (
          <p key={it.id}>
            {it.kind === "approval" ? `Allow ${it.toolName}(${it.arguments})?` : it.message}
          </p>
        ))}
        <button onClick={() => respond(interrupts.map((it) => ({ id: it.id, approved: true })))}>Allow</button>
        <button onClick={() => respond(interrupts.map((it) => ({ id: it.id, approved: false })))}>Reject</button>
      </div>
    ),
  });
  return null;
}
```

Render it inside the `CopilotKitProvider`. Pass `renderInChat: false` to place
the element yourself; the hook then returns it.

A page that opens a thread while the agent is already waiting finds the
interrupts in `page.run.interrupts`. Answer them with the agent:

```ts
import { readInterrupt, toResumeEntries } from "@hastekit/copilotkit";

const waiting = (page.run?.interrupts ?? []).map(readInterrupt);
await agent.resume(toResumeEntries(waiting, decisions));
```

## Client tools

Register tools with CopilotKit as usual, and let the agent run them:

```tsx
import { useFrontendTool } from "@copilotkit/react-core/v2";
import type { HastekitAgent } from "@hastekit/copilotkit";
import { useHastekitClientTools } from "@hastekit/copilotkit/react";

function Tools({ agent, pendingToolCallIds }: { agent: HastekitAgent; pendingToolCallIds?: string[] }) {
  // pendingToolCallIds: what the opened thread is paused on (page.run?.pendingToolCallIds).
  useHastekitClientTools(agent, { pendingToolCallIds });
  useFrontendTool({
    name: "get_selection",
    description: "Read the text the user has selected.",
    handler: async () => window.getSelection()?.toString() ?? "",
  });
  return null;
}
```

In a run the agent starts, it runs a tool as soon as its call has streamed in
and sends the result as an ordinary tool message on the thread at once. A run
waiting for it takes it; a run that has paused on the call resumes with it.
Either way the chat shows one run with the tool's result in it.

A run the agent joins instead (after a reload, or one another tab started) may
replay calls that were already answered, so the agent answers only the calls to
its tools still unanswered when that run ends. Pass the opened thread's
`pendingToolCallIds` so a thread paused on the client's tools — by a page that
went away before answering — is answered too (or call
`agent.answerClientTools(ids)` yourself). Requests the agent makes on its own
(steer, resume, tool results) carry the client's tools, because the server
learns them only from the request that starts a run: the hook hands the agent
CopilotKit's current tool list (`agent.clientTools`), so this holds even on a
page that has not run anything since it loaded. Without the hook, those requests
reuse the tools and context of the last run or connect CopilotKit gave the
agent. Human-in-the-loop tools (`useHumanInTheLoop`) are left to
CopilotKit, since they wait on the user.

## More

- `agent.steer(content)` sends a message into the run in flight.
- `agent.skillSelection` and `agent.mcpSelection` (`{ disable: string[] }`) turn
  off the user's own skills and MCP servers for the next runs.
- `watchRuns(connection, { cursor })` long-polls runs starting and finishing on
  the agent's threads. When one starts on the open thread, call
  `agent.joinIfIdle()` to stream it.
- `HastekitEvent` names the CUSTOM events the server emits, such as
  `SummarizationStarted`, `BackgroundTaskStarted` and `ContextUsage`, for use with
  `agent.subscribe`.

The agent is not tied to React: `@hastekit/copilotkit` works with any AG-UI
client. The hooks are in `@hastekit/copilotkit/react`.

## License

Apache-2.0

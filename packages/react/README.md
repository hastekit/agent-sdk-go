# @hastekit/react

Headless React hooks for building a chat UI against HasteKit's AG-UI endpoints.
The package owns conversation selection, sidebar data, paginated messages,
stream assembly, reconnects, stopping, approval resumes, and attachment uploads.
Your application owns markup, styles, routing, and message rendering. There is
no CopilotKit, React DOM, or CSS dependency.

This package is standalone. The embedded `pkg/agui/web` UI has not been migrated.
It is prepared for npm packaging but has not been published to a registry.

## Local development

```sh
cd packages/react
npm ci
npm test
npm run format:check
npm pack
```

Install the resulting tarball into a React application with `npm install
/path/to/hastekit-react-0.1.0.tgz`. React 18.3 and React 19 are supported peer
versions. Build output is ESM with TypeScript declarations; use an ESM-aware
bundler such as Vite or Next.js. Hooks in Next.js belong in a client component.

## Build a chat with one hook

Create the transport once outside the component, or memoize it when its settings
are dynamic. Inline hook options are supported; changing the agent, transport,
group, initial thread, history mode, or watch setting creates a fresh controller.

```tsx
import { useState } from "react";
import { createAGUITransport, useChat } from "@hastekit/react";

// Share transport configuration, but each hook owns its own conversation state.
const transport = createAGUITransport({ baseUrl: "/api/agui" });

export function Chat() {
  // The hook starts loading the sidebar and observes background run changes.
  const chat = useChat({ agent: "assistant", transport });
  const [draft, setDraft] = useState("");

  // Errors are retained in chat.error, so a rejected action does not need a second UI.
  async function submit(event: React.FormEvent) {
    event.preventDefault();
    const text = draft.trim();
    if (!text) return;
    setDraft("");
    await chat.sendMessage(text).catch(() => {});
  }

  // Sidebar rows come from persistence after the server starts a new conversation.
  return (
    <div>
      <aside aria-label="Conversations">
        <button onClick={() => chat.newThread()}>New chat</button>
        {chat.loadingThreads && <p>Loading conversations…</p>}
        {chat.threads.map((thread) => (
          <button
            key={thread.thread_id}
            aria-pressed={chat.threadId === thread.thread_id}
            onClick={() =>
              void chat.selectThread(thread.thread_id).catch(() => {})
            }
          >
            {thread.title || "Untitled conversation"}
          </button>
        ))}
      </aside>

      <main>
        {chat.error && <p role="alert">{chat.error.message}</p>}
        {chat.loadingMessages && <p>Loading messages…</p>}
        {chat.hasOlderMessages && (
          <button
            disabled={chat.loadingOlder}
            onClick={() => void chat.loadOlderMessages().catch(() => {})}
          >
            Load older messages
          </button>
        )}
        {chat.messages.map((message) => (
          <p key={message.id}>
            <strong>{message.role}: </strong>
            {typeof message.content === "string"
              ? message.content
              : "[Attachment]"}
          </p>
        ))}
        {chat.isCompacting && <p>Compacting context…</p>}
        {chat.connection === "reconnecting" && <p>Reconnecting…</p>}
        <form onSubmit={submit}>
          <input
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
          />
          <button disabled={chat.connection !== "idle" || chat.loadingMessages}>
            Send
          </button>
          {chat.isRunning && (
            <button
              type="button"
              disabled={chat.isStopping}
              onClick={() => void chat.stop().catch(() => {})}
            >
              Stop
            </button>
          )}
        </form>
      </main>
    </div>
  );
}
```

Render `message.toolCalls`, `role: "tool"` results, multipart content, and
reasoning messages as appropriate for your product. The example renders plain
text only. The package never injects HTML from messages into the DOM.

## Share sidebar and chat state with context

```tsx
import { ChatProvider, useChatContext } from "@hastekit/react";

// Wrap the common ancestor once to share requests and state across components.
function ChatScreen() {
  return (
    <ChatProvider options={{ agent: "assistant", transport }}>
      <Sidebar />
      <Transcript />
      <Composer />
    </ChatProvider>
  );
}

// Every descendant reads the same controller rather than opening another stream.
function Sidebar() {
  const { threads, newThread } = useChatContext();
  return (
    <button onClick={() => newThread()}>New chat ({threads.length})</button>
  );
}
```

Use either one `useChat` owner or one `ChatProvider` per chat experience. Multiple
independent `useChat` calls intentionally create independent stores. Unmounting
or changing conversations disconnects observation; it does not stop server work.
React StrictMode effect cleanup and remount are supported. No browser globals
are accessed during rendering; network work starts in effects or user actions.

## State and actions

| State                                           | Meaning                                                  |
| ----------------------------------------------- | -------------------------------------------------------- |
| `threads`, `loadingThreads`, `threadsSupported` | Sidebar rows and availability; HTTP 501 disables listing |
| `threadId`, `sessionId`                         | Selected conversation and attachment storage session     |
| `messages`, `loadingMessages`                   | AG-UI messages in chronological order                    |
| `hasOlderMessages`, `loadingOlder`              | History pagination status                                |
| `connection`                                    | `idle`, `connecting`, `streaming`, or `reconnecting`     |
| `isRunning`, `isStopping`                       | Observed execution and pending stop request              |
| `run`                                           | Outstanding run, interrupts, and approval state          |
| `isCompacting`                                  | SDK summarization lifecycle                              |
| `state`                                         | Agent state from snapshots and JSON patches              |
| `activeThreadIds`                               | Running conversations observed through the run feed      |
| `error`, `lastEvent`                            | Latest actionable error and raw protocol event           |

| Action                                  | Behavior                                                          |
| --------------------------------------- | ----------------------------------------------------------------- |
| `newThread()`                           | Allocate a local draft; persist when its first message is sent    |
| `selectThread(id)`                      | Fetch history and attach to the current run with GET              |
| `refreshThreads()`                      | Reload sidebar rows                                               |
| `loadOlderMessages()`                   | Prepend the next page, deduplicated by message ID                 |
| `sendMessage(content, forwardedProps?)` | Display input immediately and POST one turn                       |
| `resume(decisions)`                     | Resume approvals through `forwardedProps.command.resume`          |
| `stop()`                                | POST cancellation to the server and keep reading its final events |
| `disconnect()`                          | Detach browser observation without stopping server work           |
| `reconnect()`                           | Reload persisted history and replay the current run               |
| `uploadAttachment(file)`                | Upload to the selected session; returns file metadata             |
| `clearError()`                          | Dismiss the current error                                         |

Sending while a stream is attached is rejected; disable the composer until
`connection === "idle"`. This first version does not implement steering an
active run. Sidebar rename/delete and client-side tool execution are also not
implemented because the default SDK endpoints do not provide these operations.
Attachment uploads return metadata; applications choose how to construct
multipart message content. Approval decisions have `{ toolCallId, approved,
content? }`. Pass skill selection or other extensions in `forwardedProps`.

## Reconnect behavior

The initial send uses POST exactly once, including when a network failure makes
its outcome uncertain. Retries attach with GET and `Last-Event-ID`; complete
frames alone advance the cursor. Duplicate event IDs are discarded. Cursors are
kept only for the active connection: a browser reload restores history and
replays the run instead of mixing a persisted cursor with an incomplete state.

EOF before `RUN_FINISHED` or `RUN_ERROR` triggers reconnection. Defaults are a
45-second idle watchdog, eight consecutive retries, and exponential backoff
starting at 500 ms and capped at 10 seconds. Accepted events reset the retry
count. A missing replay stream reloads persisted history. Authentication and
other non-retryable HTTP errors are surfaced immediately. A stop request failing
never silently pretends the server run was cancelled.

Both current-run events and the optional `/runs` long-poll feed refresh sidebar
rows. Disable `watchRuns` for backends without that endpoint; HTTP 501 also
terminates the feed. The server resolves authentication and namespace; the client
does not accept a namespace override. `groupId` filters the sidebar, while the
SDK currently creates ordinary new runs in its default group.

## Configure authentication or another backend

```ts
// Resolve credentials for every request, including stream reconnects.
const transport = createAGUITransport({
  baseUrl: "https://example.com/api/agui",
  credentials: "include",
  headers: async () => ({ Authorization: `Bearer ${await getAccessToken()}` }),
  pageSize: 50,
  maxRetries: 8,
});
```

Provide `fetch` for instrumentation or custom request handling. Cross-origin
servers must permit your origin, auth headers, and `Last-Event-ID` through CORS.
Use an absolute `baseUrl` for non-browser fetch implementations. Each concurrent
SSR request must own its controller and authentication configuration.

Implement `ChatTransport` for another backend. The React hooks depend only on
that interface, not URL conventions. `ChatController` is exported for direct
non-React integration and deterministic testing. Its `mount()` returns cleanup;
its `subscribe()` and `getSnapshot()` follow the external-store contract.

Use `onEvent` for custom tool progress, generated-file notifications, background
tasks, or other application extensions. The reducer handles text, reasoning
text, tool calls/results, message snapshots, state snapshots/deltas, lifecycle,
input echoes, approvals, and compaction. Unhandled protocol events are retained
in `lastEvent` and delivered to `onEvent` without changing the transcript.

## Agent skills

The client loads the agent's catalog from `GET /agents/{agent}/skills` on mount.
`skills` contains names, descriptions, host policy, and effective `enabled` state.
Use `loadingSkills`, `skillsError`, and `refreshSkills()` for the picker lifecycle.
A catalog failure does not prevent chatting; custom transports may omit `listSkills`.

```tsx
const chat = useChatContext();

// Render optional choices and keep host-required skills enabled.
return chat.skills.map((skill) => (
  <label key={skill.name}>
    <input
      type="checkbox"
      checked={skill.enabled}
      disabled={skill.required}
      onChange={(event) =>
        chat.setSkillEnabled(skill.name, event.target.checked)
      }
    />
    {skill.name}
  </label>
));
```

Selections apply to subsequent sends and resumes in this controller, including
new conversations. They do not alter an active run or server configuration, and
are not persisted across page reloads. `resetSkills()` restores catalog defaults.
Refresh retains valid optional choices and removes unavailable or required overrides.

The client sends deviations from defaults as `forwardedProps.skills` with
`enable` and `disable` arrays. Unrelated forwarded properties are preserved.
Explicit per-message forwarded properties take precedence, so a caller can use
`sendMessage("Review this", { skills: { enable: ["review"], disable: [] } })`
for a single run without changing the picker's selection. If no picker overrides
exist, configured `forwardedProps.skills` is passed through unchanged; the picker
reflects catalog defaults, not manually supplied forwarded properties.

## Routines

`useRoutines({ transport })` manages scheduled work independently of a chat
provider. Memoize the transport as with `useChat`. The hook loads `routines` and
eligible `agents`, exposes `loading`, `busy`, and `error`, and provides `refresh`,
`create`, `update`, `delete`, `setEnabled`, and `runNow` actions. Mutations update
the local list from the server response and reject overlapping operations.
Errors are exposed in state and rejected to the caller. Writes are never retried.

```tsx
const transport = useMemo(
  () => createAGUITransport({ baseUrl: "/api/agui" }),
  [],
);
const routines = useRoutines({ transport });

// Create a recurring instruction using an eligible agent from routines.agents.
await routines.create({
  name: "Morning summary",
  agent: "Root_Agent",
  instruction: "Summarize today's priorities.",
  schedule: { cron: "0 9 * * 1-5", timezone: "Asia/Kolkata" },
});

// One-time routines instead use schedule: { at: "2027-01-01T09:00:00+05:30" }.
await routines.setEnabled(routineId, true);
const queuedRun = await routines.runNow(routineId);
```

Pass `false` to `setEnabled` to pause a routine. A successful `runNow` means queued,
not completed. The server
must have a running scheduler for manual runs and status queries.

`transport.routines` also provides `get(id)`, `status(id)`, and `threads(id)`.
Every transport method accepts an optional final `AbortSignal` and uses the same
credentials and rotating headers as chat. Routine history returns chat `Thread`
objects: open one with its `agent_name` (falling back to the routine's agent),
`initialThreadId: thread.thread_id`, and `groupId: routine.id` in `useChat` or
`ChatProvider`. This preserves the original agent attribution after routine edits.

The server must enable `agui.WithRoutines`. Missing endpoints and scheduler errors
are surfaced instead of treated as an empty list. Custom chat transports can
implement the optional `RoutineTransport` interface at `transport.routines`.
The hook does not poll automatically; refresh lists and status when appropriate
for your application.

### Live routine conversations

`useRoutineThreads({ transport, routineId, agent })` exposes `threads`, `loading`,
`error`, and `refresh()`. It loads occurrence history and watches lifecycle events
for the routine's agent (plus agents attributed to existing occurrences). Matching
run-start and run-finish notifications automatically refresh the list. Cleanup
cancels history requests and watchers. For transports or servers without run-feed
support, it falls back to refreshing every five seconds.

import { useCallback, useEffect, useMemo, useState } from "react";
import {
  CopilotKitProvider,
  CopilotChat,
  CopilotChatInput,
  useDefaultRenderTool,
  useInterrupt,
} from "@copilotkit/react-core/v2";
import { StoppableHttpAgent } from "./stoppable-agent";
import type { Message as AGUIMessage } from "@ag-ui/core";
import {
  fetchAgents,
  fetchThreads,
  fetchMessages,
  runUrl,
  relativeTime,
  type ThreadInfo,
} from "./api";

// App drives the SDK's AG-UI endpoints through CopilotKit v2 + an
// @ag-ui/client HttpAgent registered via `selfManagedAgents`. A
// sidebar lists stored conversations (from the /threads endpoint) and
// resumes them by hydrating the agent with the thread's history.
//
// HITL goes through CopilotKit's first-class useInterrupt hook — the
// server emits a CUSTOM "on_interrupt" event carrying the pending tool
// calls, the hook renders an approval card inline, and submitting
// fires a fresh run with forwardedProps.command.resume that the server
// parses back into a tool-approval response.

// Active is the chat surface's state: the thread we POST to plus the
// history to hydrate it with. conversationId is informational.
interface Active {
  threadId: string;
  initialMessages: AGUIMessage[];
}

function newActive(): Active {
  return { threadId: crypto.randomUUID(), initialMessages: [] };
}

export default function App() {
  const [agents, setAgents] = useState<string[]>([]);
  const [agentName, setAgentName] = useState<string>("");
  const [active, setActive] = useState<Active>(() => newActive());
  const [threads, setThreads] = useState<ThreadInfo[]>([]);
  const [listingSupported, setListingSupported] = useState(true);
  const [error, setError] = useState<string | null>(null);
  // runError holds the message from a failed run (server RUN_ERROR or a
  // transport failure). CopilotKit only logs these to the console, so we
  // surface them as a banner in the chat pane. Cleared when a new run
  // starts or the thread/agent changes.
  const [runError, setRunError] = useState<string | null>(null);

  // Load the agent list once.
  useEffect(() => {
    fetchAgents()
      .then((names) => {
        setAgents(names);
        if (names.length) setAgentName(names[0]);
        else setError("No agents registered on the server.");
      })
      .catch((e) => setError(String(e)));
  }, []);

  const refreshThreads = useCallback(async () => {
    if (!agentName) return;
    try {
      const res = await fetchThreads(agentName);
      setListingSupported(res.supported);
      setThreads(res.threads);
    } catch (e) {
      setError(String(e));
    }
  }, [agentName]);

  useEffect(() => {
    refreshThreads();
  }, [refreshThreads]);

  // Fresh agent per (agent, thread). The provider re-keys on threadId
  // below so the whole chat subtree re-initialises cleanly when the user
  // switches conversations — no leaked in-flight stream or pending
  // interrupt from the prior thread.
  //
  // StoppableHttpAgent so the stop button ends the run server-side rather
  // than only dropping the stream.
  //
  // The thread's history goes in at construction. CopilotChat rejoins the
  // thread itself (it connects whenever it is given an explicit threadId),
  // and that connect clears the agent's messages first — so hydrating from
  // here afterwards is a race we lose. The agent re-seeds its own history
  // on run initialization instead; see StoppableHttpAgent.
  const agent = useMemo(() => {
    if (!agentName) return null;
    return new StoppableHttpAgent({
      agentName,
      url: runUrl(agentName),
      threadId: active.threadId,
      history: active.initialMessages,
    });
  }, [agentName, active.threadId, active.initialMessages]);

  // Rejoining a run in flight is CopilotChat's own doing: it connects to
  // the thread whenever it is given an explicit threadId, and the server
  // replays the run so far before following it live. Nothing to start from
  // here — the agent only has to survive the clear that connect does first
  // (see StoppableHttpAgent), which is why history is handed to it above
  // rather than pushed in with setMessages after mount.

  // Refresh the sidebar after each run finishes — that's when the
  // thread row is created/updated server-side. Also surface run errors:
  // the server emits RUN_ERROR (onRunErrorEvent) for agent/LLM failures,
  // and the client raises onRunFailed for transport errors — CopilotKit
  // only console.errors both, so we lift them into a banner.
  useEffect(() => {
    if (!agent) return;
    agent.subscribe({
      onRunInitialized: () => setRunError(null),
      onRunFinalized: () => refreshThreads(),
      onRunErrorEvent: ({ event }: any) =>
        setRunError(event?.message || "The agent run failed."),
      onRunFailed: ({ error }: any) =>
        setRunError(error?.message || String(error) || "The agent run failed."),
    });
    // HttpAgent 0.0.53 has no unsubscribe handle; the agent is replaced
    // by useMemo when threadId changes, dropping the subscription.
  }, [agent, refreshThreads]);

  // Clear a stale run error when the user switches thread or agent.
  useEffect(() => setRunError(null), [active.threadId, agentName]);

  // Steering: a turn typed while the agent is working folds into the run
  // in flight (see StoppableHttpAgent.steer). Memoised so the composer
  // isn't remounted on every render.
  const steer = useCallback((text: string) => void agent?.steer(text), [agent]);
  // Cast: the slot type expects CopilotChatInput's own static sub-slots on
  // whatever it is handed. This wrapper only changes behaviour and renders
  // the real composer, so it has none of them and needs none.
  const inputSlot = useMemo(
    () => ((p: any) => <SteerableInput {...p} onSteer={steer} />) as any,
    [steer]
  );

  const selectThread = useCallback(
    async (t: ThreadInfo) => {
      if (t.thread_id === active.threadId) return;
      try {
        const messages = await fetchMessages(agentName, t.thread_id);
        setActive({
          threadId: t.thread_id,
          initialMessages: messages,
        });
      } catch (e) {
        setError(String(e));
      }
    },
    [agentName, active.threadId]
  );

  const startNewChat = useCallback(() => setActive(newActive()), []);

  const onAgentChange = useCallback((name: string) => {
    setAgentName(name);
    setListingSupported(true);
    setActive(newActive());
  }, []);

  return (
    // data-copilotkit + .dark put this whole tree in CopilotKit v2's
    // dark token scope (its tokens are defined on `[data-copilotkit].dark`).
    // The chat gets its own scope from CopilotKitProvider; setting it here
    // too means the sidebar — which lives OUTSIDE the provider — sees the
    // same --sidebar/--background/--border/... tokens and matches the chat.
    <div className="dark app" data-copilotkit>
      <Sidebar
        agents={agents}
        agentName={agentName}
        onAgentChange={onAgentChange}
        threads={threads}
        activeThreadId={active.threadId}
        onSelect={selectThread}
        onNew={startNewChat}
        listingSupported={listingSupported}
        error={error}
      />
      {agent && (
        <CopilotKitProvider
          key={active.threadId}
          selfManagedAgents={{ [agentName]: agent }}
          showDevConsole={false}
        >
          <div className="chat-pane">
            <InterruptHandler agentName={agentName} />
            <InlineToolRenderer agentName={agentName} />
            {runError && (
              <div className="run-error" role="alert">
                <span className="ico">⚠</span>
                <div className="msg">{runError}</div>
                <button
                  className="dismiss"
                  onClick={() => setRunError(null)}
                  aria-label="Dismiss error"
                >
                  ×
                </button>
              </div>
            )}
            <div className="chat-inner">
              <CopilotChat
                agentId={agentName}
                threadId={active.threadId}
                labels={{ chatInputPlaceholder: "Talk to the agent…" }}
                input={inputSlot}
              />
            </div>
          </div>
        </CopilotKitProvider>
      )}
    </div>
  );
}

// ── Composer ───────────────────────────────────────────────

// SteerableInput is the composer with one change: while a run is in
// flight, typing turns Send back into Send — the text folds into the
// running turn instead of stopping it. With an empty box the button stays
// Stop, so nothing is taken away.
//
// Done by telling CopilotChatInput the run isn't in flight rather than by
// intercepting the click: `isProcessing` is what makes it draw the square
// AND what routes both the button and the Enter key to onStop, so flipping
// it fixes the icon and the keyboard in one move. onSubmitMessage then
// routes the text to the run that is actually running.
function SteerableInput(props: any) {
  // onSteer is ours; CopilotChatInput spreads what it doesn't recognise
  // onto its DOM node.
  const { onSteer, ...inputProps } = props;

  const hasText = ((props.value ?? "") as string).trim().length > 0;
  const steering = !!props.isRunning && !!onSteer && hasText;

  return (
    <CopilotChatInput
      {...inputProps}
      isRunning={props.isRunning && !steering}
      onSubmitMessage={(text: string) => {
        if (props.isRunning && onSteer && text.trim()) {
          onSteer(text);
          return;
        }
        props.onSubmitMessage?.(text);
      }}
    />
  );
}

// ── Sidebar ────────────────────────────────────────────────

function Sidebar({
  agents,
  agentName,
  onAgentChange,
  threads,
  activeThreadId,
  onSelect,
  onNew,
  listingSupported,
  error,
}: {
  agents: string[];
  agentName: string;
  onAgentChange: (name: string) => void;
  threads: ThreadInfo[];
  activeThreadId: string;
  onSelect: (t: ThreadInfo) => void;
  onNew: () => void;
  listingSupported: boolean;
  error: string | null;
}) {
  return (
    <aside className="sidebar">
      <div className="brand">
        <span className="mark">⚡</span> HasteKit <span className="sub">AG-UI</span>
      </div>
      <div className="side-controls">
        {agents.length > 1 && (
          <select value={agentName} onChange={(e) => onAgentChange(e.target.value)}>
            {agents.map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        )}
        <button className="primary" onClick={onNew}>
          + New chat
        </button>
      </div>
      <div className="thread-list">
        {error && <div className="hint error">{error}</div>}
        {!listingSupported && (
          <div className="hint">Conversation history is not available for this agent.</div>
        )}
        {listingSupported && !error && threads.length === 0 && (
          <div className="hint">No conversations yet — start a new chat.</div>
        )}
        {threads.map((t) => (
          <button
            key={t.thread_id}
            className={"thread-item" + (t.thread_id === activeThreadId ? " selected" : "")}
            onClick={() => onSelect(t)}
          >
            <div className="title">{t.title || "Untitled"}</div>
            <div className="time">{relativeTime(t.updated_at)}</div>
          </button>
        ))}
      </div>
    </aside>
  );
}

// ── HITL approval ──────────────────────────────────────────

interface ApprovalDecision {
  toolCallId: string;
  approved: boolean;
}

interface PendingToolCall {
  toolCallId: string;
  toolCallName: string;
  arguments: string;
}

interface InterruptPayload {
  kind: string;
  pendingToolCalls?: PendingToolCall[];
}

function InterruptHandler({ agentName }: { agentName: string }) {
  useInterrupt<ApprovalDecision[]>({
    agentId: agentName,
    enabled: (event: any) => {
      const v = event?.value as InterruptPayload | undefined;
      return v?.kind === "tool_approval";
    },
    render: ({ event, resolve }: any) => {
      const payload = event.value as InterruptPayload;
      return (
        <ApprovalCard
          calls={payload.pendingToolCalls ?? []}
          onSubmit={(decisions) => {
            // useInterrupt forwards resolve()'s argument verbatim under
            // forwardedProps.command.resume on the next run. Wrap as
            // { decisions } so the server's canonical parse path
            // (command.resume.decisions) picks it up.
            resolve({ decisions } as any);
          }}
        />
      );
    },
  });
  return null;
}

function ApprovalCard({
  calls,
  onSubmit,
}: {
  calls: PendingToolCall[];
  onSubmit: (decisions: ApprovalDecision[]) => void;
}) {
  const [decisions, setDecisions] = useState<Record<string, boolean>>(() => {
    const d: Record<string, boolean> = {};
    for (const c of calls) d[c.toolCallId] = true;
    return d;
  });
  const approved = Object.values(decisions).filter(Boolean).length;

  return (
    <div className="hk-approval">
      <h4>
        ⏸ Approve {calls.length} pending tool call{calls.length === 1 ? "" : "s"}
      </h4>
      {calls.map((c) => (
        <label className="hk-call" key={c.toolCallId}>
          <input
            type="checkbox"
            checked={decisions[c.toolCallId] ?? true}
            onChange={(e) =>
              setDecisions((p) => ({ ...p, [c.toolCallId]: e.target.checked }))
            }
          />
          <div className="meta">
            <div className="nm">{c.toolCallName}</div>
            <div className="args" title={c.arguments}>
              {c.arguments}
            </div>
          </div>
        </label>
      ))}
      <div className="hk-actions">
        <span className="count">
          {approved} of {calls.length} approved
        </span>
        <button
          className="hk-btn"
          onClick={() =>
            onSubmit(calls.map((c) => ({ toolCallId: c.toolCallId, approved: false })))
          }
        >
          Reject all
        </button>
        <button
          className="hk-btn primary"
          onClick={() =>
            onSubmit(
              calls.map((c) => ({
                toolCallId: c.toolCallId,
                approved: decisions[c.toolCallId] ?? true,
              }))
            )
          }
        >
          Submit
        </button>
      </div>
    </div>
  );
}

// ── Tool call rendering ────────────────────────────────────

// InlineToolRenderer registers a wildcard renderer so every tool call
// gets our collapsible card instead of CopilotKit's default.
function InlineToolRenderer({ agentName }: { agentName: string }) {
  useDefaultRenderTool(
    {
      render: (props: any) => <ToolCallCard {...props} />,
    },
    [agentName]
  );
  return null;
}

function ToolCallCard({
  name,
  status,
  parameters,
  result,
}: {
  name: string;
  status: "inProgress" | "executing" | "complete";
  parameters: unknown;
  result: string | undefined;
}) {
  const dot =
    status === "complete" ? "#10b981" : status === "executing" ? "#f59e0b" : "#94a3b8";
  const pill =
    status === "complete"
      ? { label: "Done", bg: "#dcfce7", fg: "#166534" }
      : status === "executing"
      ? { label: "Running", bg: "#fef3c7", fg: "#854d0e" }
      : { label: "Pending", bg: "#f1f5f9", fg: "#475569" };

  return (
    <div className="hk-tool">
      <details>
        <summary>
          <span className="hk-dot" style={{ background: dot }} />
          <code>{name}</code>
          <span className="hk-pill" style={{ background: pill.bg, color: pill.fg }}>
            {pill.label}
          </span>
        </summary>
        <div className="body">
          {hasContent(parameters) && <Block label="Arguments" value={parameters} />}
          {result && <Block label="Result" value={result} />}
        </div>
      </details>
    </div>
  );
}

function Block({ label, value }: { label: string; value: unknown }) {
  const text = typeof value === "string" ? value : safePretty(value);
  const clipped = text.length > 800 ? text.slice(0, 800) + "…" : text;
  return (
    <div>
      <div className="blk-label">{label}</div>
      <pre>{clipped}</pre>
    </div>
  );
}

function hasContent(v: unknown): boolean {
  if (v == null) return false;
  if (typeof v === "string") return v.length > 0;
  if (Array.isArray(v)) return v.length > 0;
  if (typeof v === "object") return Object.keys(v as object).length > 0;
  return true;
}

function safePretty(v: unknown): string {
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}

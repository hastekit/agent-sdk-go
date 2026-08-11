import { useCallback, useEffect, useMemo, useRef, useState } from "react";
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
  const [sidebarOpen, setSidebarOpen] = useState(true);

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
    <div
      className={"dark app" + (sidebarOpen ? "" : " sidebar-hidden")}
      data-copilotkit
    >
      <Sidebar
        threads={threads}
        activeThreadId={active.threadId}
        onSelect={selectThread}
        onNew={startNewChat}
        onCollapse={() => setSidebarOpen(false)}
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
            <header className="topbar">
              {!sidebarOpen && (
                <button
                  className="icon-btn"
                  onClick={() => setSidebarOpen(true)}
                  aria-label="Show sidebar"
                >
                  <PanelIcon />
                </button>
              )}
              <AgentMenu
                agents={agents}
                agentName={agentName}
                onAgentChange={onAgentChange}
              />
            </header>
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
                labels={{
                  chatInputPlaceholder: "Ask anything",
                  chatDisclaimerText:
                    "The agent can make mistakes. Check important info.",
                }}
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

// Where the logo lives at runtime. public/ is copied to the build output
// as-is, and BASE_URL keeps the reference relative so the UI still finds
// it when the Go server is mounted under a sub-path.
const LOGO = `${import.meta.env.BASE_URL}hastekit-logo.svg`;

function Sidebar({
  threads,
  activeThreadId,
  onSelect,
  onNew,
  onCollapse,
  listingSupported,
  error,
}: {
  threads: ThreadInfo[];
  activeThreadId: string;
  onSelect: (t: ThreadInfo) => void;
  onNew: () => void;
  onCollapse: () => void;
  listingSupported: boolean;
  error: string | null;
}) {
  return (
    <aside className="sidebar">
      <div className="side-head">
        <span className="brand">
          <img className="logo" src={LOGO} alt="" />
          HasteKit
        </span>
        <button className="icon-btn" onClick={onCollapse} aria-label="Hide sidebar">
          <PanelIcon />
        </button>
      </div>

      <nav className="side-nav">
        <button className="nav-item" onClick={onNew}>
          <ComposeIcon />
          New chat
        </button>
      </nav>

      <div className="thread-list">
        <div className="section-label">Recents</div>
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
            title={`${t.title || "Untitled"} · ${relativeTime(t.updated_at)}`}
          >
            {t.title || "Untitled"}
          </button>
        ))}
      </div>
    </aside>
  );
}

// ── Agent selector ─────────────────────────────────────────

// The title-bar dropdown: the agent the chat is pointed at. A menu rather
// than a <select> so it can carry the same weight as the rest of the bar —
// and so a single registered agent reads as a heading, not a control.
function AgentMenu({
  agents,
  agentName,
  onAgentChange,
}: {
  agents: string[];
  agentName: string;
  onAgentChange: (name: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const wrap = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (!wrap.current?.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  const single = agents.length < 2;

  return (
    <div className="agent-menu" ref={wrap}>
      <button
        className="agent-trigger"
        onClick={() => !single && setOpen((v) => !v)}
        aria-haspopup={single ? undefined : "menu"}
        aria-expanded={single ? undefined : open}
        disabled={single}
      >
        {agentName || "No agent"}
        {!single && <ChevronIcon />}
      </button>

      {open && (
        <div className="agent-pop" role="menu">
          {agents.map((n) => (
            <button
              key={n}
              role="menuitem"
              className={"agent-opt" + (n === agentName ? " selected" : "")}
              onClick={() => {
                setOpen(false);
                if (n !== agentName) onAgentChange(n);
              }}
            >
              <span className="nm">{n}</span>
              {n === agentName && <CheckIcon />}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

// ── Icons ──────────────────────────────────────────────────

const svg = {
  width: 18,
  height: 18,
  viewBox: "0 0 24 24",
  fill: "none",
  stroke: "currentColor",
  strokeWidth: 1.6,
  strokeLinecap: "round" as const,
  strokeLinejoin: "round" as const,
};

function PanelIcon() {
  return (
    <svg {...svg}>
      <rect x="3" y="4" width="18" height="16" rx="2" />
      <path d="M9 4v16" />
    </svg>
  );
}

function ComposeIcon() {
  return (
    <svg {...svg}>
      <path d="M12 4H6a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-6" />
      <path d="M18.5 3.5a2.1 2.1 0 0 1 3 3L12 16l-4 1 1-4z" />
    </svg>
  );
}

function ChevronIcon() {
  return (
    <svg {...svg} width={16} height={16}>
      <path d="m6 9 6 6 6-6" />
    </svg>
  );
}

function CheckIcon() {
  return (
    <svg {...svg} width={16} height={16}>
      <path d="m5 13 4 4L19 7" />
    </svg>
  );
}

// ── HITL: approvals and elicitations ───────────────────────

// A run pauses in one of three ways, and each needs a different thing from
// the user: approve/reject a call, fill in a form, or visit a URL. They
// arrive on the same on_interrupt event and can be mixed in one pause, so
// one card collects them all and resolves once.

interface ApprovalDecision {
  toolCallId: string;
  approved: boolean;
  content?: Record<string, unknown>;
}

interface PendingToolCall {
  toolCallId: string;
  toolCallName: string;
  arguments: string;
}

interface SchemaProperty {
  type?: string;
  title?: string;
  description?: string;
  enum?: string[];
  default?: unknown;
}

interface RequestedSchema {
  properties?: Record<string, SchemaProperty>;
  required?: string[];
}

interface InterruptEntry {
  toolCallId: string;
  toolCallName?: string;
  arguments?: string;
  mode?: string;
  message?: string;
  requestedSchema?: RequestedSchema;
  url?: string;
}

interface InterruptPayload {
  kind: string;
  pendingToolCalls?: PendingToolCall[];
  interrupts?: InterruptEntry[];
}

function InterruptHandler({ agentName }: { agentName: string }) {
  useInterrupt<ApprovalDecision[]>({
    agentId: agentName,
    enabled: (event: any) => {
      const v = event?.value as InterruptPayload | undefined;
      return (
        v?.kind === "tool_approval" ||
        v?.kind === "elicitation" ||
        v?.kind === "mixed"
      );
    },
    render: ({ event, resolve }: any) => {
      const payload = event.value as InterruptPayload;
      // interrupts carries every mode; pendingToolCalls is the older
      // approval-only shape, kept working for servers that predate it.
      const entries: InterruptEntry[] = payload.interrupts?.length
        ? payload.interrupts
        : (payload.pendingToolCalls ?? []).map((c) => ({ ...c, mode: "approval" }));
      return (
        <InterruptCard
          entries={entries}
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

// coerce turns a form input's string back into the type the schema asked
// for, so a number field does not arrive at the server quoted.
function coerce(raw: string, prop?: SchemaProperty): unknown {
  if (prop?.type === "number" || prop?.type === "integer") {
    if (raw.trim() === "") return undefined;
    const n = Number(raw);
    return Number.isNaN(n) ? raw : n;
  }
  return raw;
}

function initialForm(schema?: RequestedSchema): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [name, prop] of Object.entries(schema?.properties ?? {})) {
    if (prop.default !== undefined) out[name] = prop.default;
    else if (prop.type === "boolean") out[name] = false;
  }
  return out;
}

function InterruptCard({
  entries,
  onSubmit,
}: {
  entries: InterruptEntry[];
  onSubmit: (decisions: ApprovalDecision[]) => void;
}) {
  const approvals = entries.filter((e) => !e.mode || e.mode === "approval");
  const forms = entries.filter((e) => e.mode === "form");
  const urls = entries.filter((e) => e.mode === "url");

  const [checked, setChecked] = useState<Record<string, boolean>>(() => {
    const d: Record<string, boolean> = {};
    for (const e of approvals) d[e.toolCallId] = true;
    return d;
  });
  const [values, setValues] = useState<Record<string, Record<string, unknown>>>(() => {
    const d: Record<string, Record<string, unknown>> = {};
    for (const e of forms) d[e.toolCallId] = initialForm(e.requestedSchema);
    return d;
  });

  // A required field left empty would be rejected by the server's schema
  // validation, so catch it here where the user can still see the field.
  const missing = forms.some((e) =>
    (e.requestedSchema?.required ?? []).some((name) => {
      const v = values[e.toolCallId]?.[name];
      return v === undefined || v === "";
    })
  );

  const submit = (approved: boolean) =>
    onSubmit(
      entries.map((e) => {
        if (!approved) return { toolCallId: e.toolCallId, approved: false };
        if (e.mode === "form") {
          return {
            toolCallId: e.toolCallId,
            approved: true,
            content: values[e.toolCallId] ?? {},
          };
        }
        if (e.mode === "url") return { toolCallId: e.toolCallId, approved: true };
        return { toolCallId: e.toolCallId, approved: checked[e.toolCallId] ?? true };
      })
    );

  const title =
    forms.length || urls.length
      ? forms.length && !urls.length
        ? "The agent needs some details"
        : "The agent needs something from you"
      : `Approve ${approvals.length} pending tool call${approvals.length === 1 ? "" : "s"}`;

  return (
    <div className="hk-approval">
      <h4>⏸ {title}</h4>

      {approvals.map((e) => (
        <label className="hk-call" key={e.toolCallId}>
          <input
            type="checkbox"
            checked={checked[e.toolCallId] ?? true}
            onChange={(ev) =>
              setChecked((p) => ({ ...p, [e.toolCallId]: ev.target.checked }))
            }
          />
          <div className="meta">
            <div className="nm">{e.toolCallName}</div>
            <div className="args" title={e.arguments}>
              {e.arguments}
            </div>
          </div>
        </label>
      ))}

      {forms.map((e) => (
        <div className="hk-elicit" key={e.toolCallId}>
          {e.message && <p className="hk-elicit-msg">{e.message}</p>}
          {Object.entries(e.requestedSchema?.properties ?? {}).map(([name, prop]) => {
            const required = (e.requestedSchema?.required ?? []).includes(name);
            const value = values[e.toolCallId]?.[name];
            const set = (v: unknown) =>
              setValues((p) => ({
                ...p,
                [e.toolCallId]: { ...(p[e.toolCallId] ?? {}), [name]: v },
              }));
            return (
              <label className="hk-field" key={name}>
                <span className="hk-field-label">
                  {prop.title || name}
                  {required && <em className="hk-req"> *</em>}
                </span>
                {prop.enum ? (
                  <select
                    value={String(value ?? "")}
                    onChange={(ev) => set(ev.target.value)}
                  >
                    <option value="">—</option>
                    {prop.enum.map((opt) => (
                      <option key={opt} value={opt}>
                        {opt}
                      </option>
                    ))}
                  </select>
                ) : prop.type === "boolean" ? (
                  <input
                    type="checkbox"
                    checked={Boolean(value)}
                    onChange={(ev) => set(ev.target.checked)}
                  />
                ) : (
                  <input
                    type={prop.type === "number" || prop.type === "integer" ? "number" : "text"}
                    value={value === undefined ? "" : String(value)}
                    onChange={(ev) => set(coerce(ev.target.value, prop))}
                  />
                )}
                {prop.description && <span className="hk-hint">{prop.description}</span>}
              </label>
            );
          })}
        </div>
      ))}

      {urls.map((e) => (
        <div className="hk-elicit" key={e.toolCallId}>
          {e.message && <p className="hk-elicit-msg">{e.message}</p>}
          <a className="hk-link" href={e.url} target="_blank" rel="noreferrer noopener">
            {e.url}
          </a>
          <p className="hk-hint">Open the link, then continue below.</p>
        </div>
      ))}

      <div className="hk-actions">
        {approvals.length > 0 && !forms.length && !urls.length && (
          <span className="count">
            {Object.values(checked).filter(Boolean).length} of {approvals.length} approved
          </span>
        )}
        <button className="hk-btn" onClick={() => submit(false)}>
          {forms.length || urls.length ? "Cancel" : "Reject all"}
        </button>
        <button className="hk-btn primary" disabled={missing} onClick={() => submit(true)}>
          {forms.length || urls.length ? "Continue" : "Submit"}
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

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import {
  CopilotKitProvider,
  CopilotChat,
  useDefaultRenderTool,
  useInterrupt,
} from "@copilotkit/react-core/v2";
import { AttachmentMessageView } from "./attachment-message";
import { AttachmentInput } from "./attachment-input";
import type { InputContent } from "@ag-ui/core";
import { StoppableHttpAgent } from "./stoppable-agent";
import type { Message as AGUIMessage } from "@ag-ui/core";
import {
  fetchAgents,
  fetchThreads,
  fetchMessages,
  runUrl,
  relativeTime,
  watchRunFeed,
  type ThreadInfo,
  type ThreadRunState,
  type ThreadBackgroundTask,
} from "./api";

// How long the run feed holds a request open, and how long to wait after a
// failure before asking again. The wait sits comfortably inside the idle
// timeouts proxies usually impose.
const FEED_WAIT_SECONDS = 25;
const FEED_RETRY_MS = 2000;

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
  // What the thread's last run left outstanding — a decision it is waiting
  // on, tasks still working. Null for a settled thread, and for a new one.
  run: ThreadRunState | null;
}

function newActive(): Active {
  return { threadId: crypto.randomUUID(), initialMessages: [], run: null };
}

// What is on screen, in the address bar: the agent and the conversation.
//
// Without it a reload lands on the first registered agent in a brand-new empty
// chat, and what the user was looking at is only in the sidebar — which is no
// use at all when the agent is sitting on a question, since the prompt to
// answer it is on the thread they just lost. Both live in the URL rather than
// storage so a link carries the whole address, and so back/forward work.
//
// The thread belongs to the agent: a conversation is stored under the agent
// that held it, so the pair travels together and is cleared together.
const AGENT_PARAM = "agent";
const THREAD_PARAM = "thread";

function paramFromURL(name: string): string {
  return new URLSearchParams(window.location.search).get(name) ?? "";
}

// replaceState, not push: switching agent or conversation is not a navigation
// the back button should have to walk through.
function rememberParam(name: string, value: string) {
  const url = new URL(window.location.href);
  if (value) url.searchParams.set(name, value);
  else url.searchParams.delete(name);
  window.history.replaceState(null, "", url.toString());
}

// What the composer tray is showing: work still running, and a pause waiting
// on the user.
//
// Through context rather than props because the tray renders inside
// CopilotChat's input slot, and the slot is a component type — passing this
// down would give it a new identity on every change, remounting the composer
// and taking whatever the user had half-typed with it.
interface Tray {
  tasks: ThreadBackgroundTask[];
  interrupt: TrayInterrupt | null;
}

interface TrayInterrupt {
  // Which pause this is, so the publisher for one can be torn down after the
  // next has already taken its place without clearing it.
  key: string;
  entries: InterruptEntry[];
  onSubmit: (decisions: ApprovalDecision[]) => void;
}

const TrayContext = createContext<Tray>({ tasks: [], interrupt: null });

export default function App() {
  const [agents, setAgents] = useState<string[]>([]);
  const [agentName, setAgentName] = useState<string>("");
  // Whether the server needs the full message list posted on every run.
  // Reported by GET /agents; false is both the default and the common case.
  const [fullHistory, setFullHistory] = useState(false);
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

  // What the address bar asked for when the page opened, read once at the
  // first render and then consumed.
  //
  // Not read from the URL where it is needed: the effects that keep the URL in
  // step with the app run before the agent list has arrived, so by then the
  // address bar says what we defaulted to rather than what was asked for.
  // Consumed rather than kept because it is for arriving at a URL, not for
  // following the user around afterwards — a thread left in here would be
  // reopened again the next time they switched agent.
  const opened = useRef({
    agent: paramFromURL(AGENT_PARAM),
    thread: paramFromURL(THREAD_PARAM),
  });

  // The open conversation, readable from the feed loop without restarting it.
  // The loop outlives any one thread — that is the point of it — so it cannot
  // close over the thread id.
  const activeThreadIdRef = useRef(active.threadId);
  useEffect(() => {
    activeThreadIdRef.current = active.threadId;
  }, [active.threadId]);

  // Likewise the agent: useMemo replaces it whenever the thread changes, and
  // the feed loop must not be torn down and restarted each time.
  const agentRef = useRef<StoppableHttpAgent | null>(null);
 const [attachmentsEnabled, setAttachmentsEnabled] = useState(false);

  // Load the agent list once.
  useEffect(() => {
    fetchAgents()
      .then(({ agents: names, fullHistory, attachmentsEnabled }) => {
 setAttachmentsEnabled(attachmentsEnabled);
        setAgents(names);
        setFullHistory(fullHistory);
        if (!names.length) {
          setError("No agents registered on the server.");
          return;
        }
        // A name the server no longer registers falls back to the first
        // rather than erroring: the link is stale, not wrong, and an empty
        // chat against a real agent is a better landing than a dead page.
        const wanted = opened.current.agent;
        setAgentName(names.includes(wanted) ? wanted : names[0]);
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
      fullHistory,
    });
  }, [agentName, active.threadId, active.initialMessages, fullHistory]);

  useEffect(() => {
    agentRef.current = agent;
  }, [agent]);

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
      // A live run says what is happening, so the snapshot the page loaded
      // with is behind it and goes.
      //
      // On the RUN_STARTED event, not on initialization: CopilotChat connects
      // to the thread whenever it is given one, and connectAgent runs the same
      // path a real run does — so initialization fires even when there was
      // nothing to join. Clearing there wiped a restored approval card the
      // instant it was drawn. This fires only when a run is actually
      // streaming, which is the thing that supersedes it.
      onRunStartedEvent: () => setRestored(null),
      onCustomEvent: ({ event }: any) => {
        const value = event?.value ?? {};
        if (event?.name === "hastekit.background_task_started" && value.taskId) {
          setLiveTasks((current) => {
            const next = new Map(current);
            next.set(value.taskId, {
              taskId: value.taskId,
              callId: value.toolCallId,
              toolName: value.toolName,
              streamId: value.streamId,
            });
            return next;
          });
          return;
        }
        if (event?.name === "hastekit.background_task_completed" && value.taskId) {
          setLiveTasks((current) => {
            if (!current.has(value.taskId)) return current;
            const next = new Map(current);
            next.delete(value.taskId);
            return next;
          });
          // The snapshot the page opened with may also be carrying it.
          setRestored((current) => {
            if (!current?.backgroundTasks?.length) return current;
            const remaining = current.backgroundTasks.filter(
              (t) => t.taskId !== value.taskId
            );
            return { ...current, backgroundTasks: remaining };
          });
        }
      },
      onRunFinalized: () => refreshThreads(),
      onRunErrorEvent: ({ event }: any) =>
        setRunError(event?.message || "The agent run failed."),
      onRunFailed: ({ error }: any) =>
        setRunError(error?.message || String(error) || "The agent run failed."),
    });
    // HttpAgent 0.0.53 has no unsubscribe handle; the agent is replaced
    // by useMemo when threadId changes, dropping the subscription.
  }, [agent, refreshThreads]);

  // Conversations that have done something since the user last looked at
  // them. Cleared when the thread is opened, so the badge means "there is
  // something here you have not seen", not "this ran recently".
  const [unseen, setUnseen] = useState<Set<string>>(() => new Set());

  // Watch every conversation in the namespace, not just the open one.
  //
  // The per-thread watch below covers the conversation on screen. This covers
  // the rest: a background task finishing in conversation A while the user
  // reads conversation B, or a conversation that did not exist when the page
  // loaded. Neither could be reached by anything keyed to a thread.
  //
  // The cursor is what makes a run that started and ended while the tab was
  // in the background still count — the feed replays it on the next poll
  // rather than dropping it.
  useEffect(() => {
    if (!agentName) return;

    let stopped = false;
    const controller = new AbortController();

    const loop = async () => {
      let cursor = "";
      while (!stopped) {
        try {
          const seen = await watchRunFeed(
            agentName,
            cursor,
            FEED_WAIT_SECONDS,
            undefined,
            controller.signal
          );
          if (stopped) return;
          cursor = seen.cursor;

          if (seen.events.length === 0) continue;

          // A run ending is when the thread row is worth re-reading: its
          // title and timestamp are written server-side as the run saves.
          if (seen.events.some((e) => e.event === "RUN_FINISHED")) {
            refreshThreads();
          }

          // A run on the conversation the user is reading is one to join, not
          // to badge: the answer belongs on screen as it is written.
          if (seen.events.some((e) => e.event === "RUN_STARTED" && e.threadId === activeThreadIdRef.current)) {
            void agentRef.current?.joinIfIdle();
          }

          setUnseen((current) => {
            const next = new Set(current);
            let changed = false;
            for (const event of seen.events) {
              // The conversation on screen is being read as it happens;
              // badging it would only ask the user to look at what they are
              // already looking at.
              if (event.threadId === activeThreadIdRef.current) continue;
              if (next.has(event.threadId)) continue;
              next.add(event.threadId);
              changed = true;
            }
            return changed ? next : current;
          });
        } catch (error) {
          if (stopped || controller.signal.aborted) return;
          console.error("run feed failed; retrying", error);
          await new Promise((done) => setTimeout(done, FEED_RETRY_MS));
        }
      }
    };

    void loop();
    return () => {
      stopped = true;
      controller.abort();
    };
  }, [agentName, refreshThreads]);

  // Clear a stale run error when the user switches thread or agent.
  useEffect(() => setRunError(null), [active.threadId, agentName]);

  // What the thread was left waiting on, as the page found it. Held apart
  // from `active` because it is transient: the moment a run starts, the run
  // is the source of truth and this is stale.
  const [restored, setRestored] = useState<ThreadRunState | null>(null);
  useEffect(() => setRestored(active.run), [active.threadId, active.run]);

  // Tasks seen starting in this session, keyed by task id.
  //
  // The reopened snapshot only covers a conversation the page has just
  // loaded. A task that starts while the user is sitting here is not in it —
  // the run that started the task ends normally, and without this the chat
  // simply goes quiet with nothing to say why. The run's own stream announces
  // both ends, so that is what this follows.
  const [liveTasks, setLiveTasks] = useState<Map<string, ThreadBackgroundTask>>(
    () => new Map()
  );
  useEffect(() => setLiveTasks(new Map()), [active.threadId]);

  // The pause the running chat is showing, lifted out of the message list so
  // it can be drawn in the same place as a pause the page was reloaded into.
  // Cleared with the thread, like everything else keyed to one conversation.
  const [liveInterrupt, setLiveInterrupt] = useState<TrayInterrupt | null>(null);
  useEffect(() => setLiveInterrupt(null), [active.threadId]);

  // What the conversation is waiting on, however we came to know: the
  // snapshot it was opened with, plus anything seen starting since.
  const runningTasks = useMemo(() => {
    const byID = new Map<string, ThreadBackgroundTask>();
    for (const task of restored?.backgroundTasks ?? []) byID.set(task.taskId, task);
    for (const [id, task] of liveTasks) byID.set(id, task);
    return [...byID.values()];
  }, [restored, liveTasks]);

  // One tray, whichever way the pause reached us. A live one wins: it is the
  // run talking, and the snapshot the page loaded with is behind it.
  const tray = useMemo<Tray>(() => {
    if (liveInterrupt) return { tasks: runningTasks, interrupt: liveInterrupt };

    const waiting = (restored?.interrupts ?? []) as unknown as InterruptEntry[];
    if (waiting.length) {
      return {
        tasks: runningTasks,
        interrupt: {
          key: "restored",
          entries: waiting,
          onSubmit: (decisions) => {
            // Cleared first: the resume starts a run, and the run is what
            // shows what happened next.
            setRestored(null);
            agent?.resume(decisions).catch((e) => setRunError(String(e)));
          },
        },
      };
    }

    return { tasks: runningTasks, interrupt: null };
  }, [liveInterrupt, restored, runningTasks, agent]);

  // Opening a conversation is what marks it seen.
  useEffect(() => {
    setUnseen((current) => {
      if (!current.has(active.threadId)) return current;
      const next = new Set(current);
      next.delete(active.threadId);
      return next;
    });
  }, [active.threadId]);

  // Steering: a turn typed while the agent is working folds into the run
  // in flight (see StoppableHttpAgent.steer). Memoised so the composer
  // isn't remounted on every render.
  const steer = useCallback((text: string, parts: InputContent[] = []) => agent?.steer(text, parts), [agent]);
  // Cast: the slot type expects CopilotChatInput's own static sub-slots on
  // whatever it is handed. This wrapper only changes behaviour and renders
  // the real composer, so it has none of them and needs none.
  const inputSlot = useMemo(
    () => ((p: any) => <SteerableInput {...p} onSteer={steer} attachmentsEnabled={attachmentsEnabled} />) as any,
    [steer, attachmentsEnabled]
  );

  const openThread = useCallback(
    async (threadId: string) => {
      try {
        const { messages, run } = await fetchMessages(agentName, threadId);
        setActive({ threadId, initialMessages: messages, run });
      } catch (e) {
        setError(String(e));
      }
    },
    [agentName]
  );

  const selectThread = useCallback(
    async (t: ThreadInfo) => {
      if (t.thread_id === active.threadId) return;
      await openThread(t.thread_id);
    },
    [openThread, active.threadId]
  );

  // Reopen whatever the address bar names, once there is an agent to open it
  // against. Runs on load and on an agent change; a thread already open is
  // left alone so this cannot fight the sidebar.
  useEffect(() => {
    if (!agentName) return;
    const wanted = opened.current.thread;
    opened.current.thread = "";
    if (!wanted || wanted === active.threadId) return;
    void openThread(wanted);
    // active.threadId is deliberately not a dependency: this is for arriving
    // at a URL, not for following the user around after that.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agentName, openThread]);

  // And keep both pointing at whatever is open.
  useEffect(() => rememberParam(AGENT_PARAM, agentName), [agentName]);
  useEffect(() => rememberParam(THREAD_PARAM, active.threadId), [active.threadId]);

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
        unseen={unseen}
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
            <InterruptHandler agentName={agentName} publish={setLiveInterrupt} />
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
            <TrayContext.Provider value={tray}>
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
                  messageView={AttachmentMessageView as any}
                />
              </div>
            </TrayContext.Provider>
          </div>
        </CopilotKitProvider>
      )}
    </div>
  );
}

// ── Composer ───────────────────────────────────────────────

// Keep the task/approval tray above the attachment-aware composer.
function SteerableInput(props: any) {
  const tray = useContext(TrayContext);
  return <><ComposerTray tray={tray} /><AttachmentInput {...props} /></>;

}

// ComposerTray is the one place the chat says what it is waiting on — a tool
// still working, a decision it needs — directly above the box the user would
// answer in.
//
// It renders inside the composer's slot because that is where the answer is
// given. It used to be two places: an approval that arrived live was drawn
// inline among the messages by CopilotKit, and the same approval after a
// reload was drawn above the transcript, so the same question moved depending
// on how the page came to know about it. It also puts the note within reach
// of a long conversation, where the top of the transcript is nowhere near
// where the user is reading.
//
// Sitting in CopilotChat's input overlay means the transcript's bottom padding
// tracks it — the overlay is measured — so nothing is left hidden behind it.
function ComposerTray({ tray }: { tray: Tray }) {
  if (!tray.tasks.length && !tray.interrupt) return null;

  return (
    <div className="composer-tray">
      {tray.tasks.length > 0 && (
        // Keyed by the tasks it is about, so dismissing it hides that set and
        // a job starting later says so rather than staying silent because the
        // note was waved away once.
        <BackgroundTaskNote
          key={tray.tasks.map((t) => t.taskId).join(",")}
          tasks={tray.tasks}
        />
      )}
      {tray.interrupt && (
        <InterruptCard
          key={tray.interrupt.key}
          entries={tray.interrupt.entries}
          onSubmit={tray.interrupt.onSubmit}
        />
      )}
    </div>
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
  unseen,
  onSelect,
  onNew,
  onCollapse,
  listingSupported,
  error,
}: {
  threads: ThreadInfo[];
  activeThreadId: string;
  unseen: Set<string>;
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
        {threads.map((t) => {
          const hasUnseen = unseen.has(t.thread_id);
          return (
            <button
              key={t.thread_id}
              className={
                "thread-item" +
                (t.thread_id === activeThreadId ? " selected" : "") +
                (hasUnseen ? " unseen" : "")
              }
              onClick={() => onSelect(t)}
              title={
                `${t.title || "Untitled"} · ${relativeTime(t.updated_at)}` +
                (hasUnseen ? " · new activity" : "")
              }
            >
              <span className="thread-title">{t.title || "Untitled"}</span>
              {hasUnseen && <span className="thread-dot" aria-label="New activity" />}
            </button>
          );
        })}
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

// InterruptHandler turns CopilotKit's inline interrupt into a tray entry.
//
// useInterrupt renders where it is asked to, which is among the messages. That
// is one of the two places an approval used to appear, and the one that moved
// when the page was reloaded. So the render hands the pause upward and draws
// nothing itself; the tray decides where it goes.
function InterruptHandler({
  agentName,
  publish,
}: {
  agentName: string;
  publish: (next: (current: TrayInterrupt | null) => TrayInterrupt | null) => void;
}) {
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
        <InterruptPublisher entries={entries} resolve={resolve} publish={publish} />
      );
    },
  });
  return null;
}

// InterruptPublisher is the pause, held as state for as long as CopilotKit
// keeps it mounted, and drawn elsewhere.
//
// A component rather than a call in render because publishing is a state
// change, and the mount/unmount pair is exactly the lifetime the pause has.
function InterruptPublisher({
  entries,
  resolve,
  publish,
}: {
  entries: InterruptEntry[];
  resolve: (value: unknown) => void;
  publish: (next: (current: TrayInterrupt | null) => TrayInterrupt | null) => void;
}) {
  // resolve is rebuilt on each of CopilotKit's renders; the tray holds one
  // callback for the life of the pause, so it reaches the current one here
  // instead of being republished to keep up.
  const resolveRef = useRef(resolve);
  useEffect(() => {
    resolveRef.current = resolve;
  });

  const key = entries.map((e) => e.toolCallId).join(",");
  useEffect(() => {
    publish(() => ({
      key,
      entries,
      onSubmit: (decisions) => {
        // useInterrupt forwards resolve()'s argument verbatim under
        // forwardedProps.command.resume on the next run. Wrap as
        // { decisions } so the server's canonical parse path
        // (command.resume.decisions) picks it up.
        resolveRef.current({ decisions } as any);
      },
    }));
    // Only if it is still ours: a pause resolved into another pause unmounts
    // this one after the next has already published, and a blind clear would
    // take the new one down with it.
    return () => publish((current) => (current?.key === key ? null : current));
    // entries is fixed for a given set of call ids, which is what key is.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, publish]);

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

// BackgroundTaskNote says a tool is still working after the turn that called
// it finished.
//
// Without it a reloaded page reads as though the agent simply stopped
// talking: the tool answered, the run ended, and the actual work is somewhere
// else entirely. The note is dismissible because the task may well outlast
// the user's interest in being told about it.
function BackgroundTaskNote({ tasks }: { tasks: ThreadBackgroundTask[] }) {
  const [hidden, setHidden] = useState(false);
  if (hidden) return null;

  return (
    <div className="background-note" role="status">
      <span className="spinner" aria-hidden="true" />
      <div className="msg">
        {tasks.length === 1
          ? `${tasks[0].toolName || "A background job"} is still running. Its result will arrive here when it finishes.`
          : `${tasks.length} background jobs are still running. Their results will arrive here when they finish.`}
      </div>
      <button
        className="dismiss"
        onClick={() => setHidden(true)}
        aria-label="Dismiss"
      >
        ×
      </button>
    </div>
  );
}

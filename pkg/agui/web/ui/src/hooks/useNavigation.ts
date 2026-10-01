import type { Message } from "@ag-ui/core";
import { fetchThreadMessages, type ContextUsage, type ThreadRunState } from "@hastekit/copilotkit";
import { useCallback, useEffect, useRef, useState } from "react";
import { fetchRoutine, fetchRoutineThreads, type Routine, type ThreadInfo } from "../api";
import { AGENT_PARAM, ROUTINE_PARAM, THREAD_PARAM, paramFromURL, rememberParam } from "../url-params";

// Active is the chat surface's state: the thread we POST to plus the
// history to hydrate it with. sessionId selects the shared attachment directory.
export interface Active {
  threadId: string;
  sessionId: string;
  initialMessages: Message[];
  nextCursor?: string;
  // What the thread's last run left outstanding — a decision it is waiting
  // on, tasks still working. Null for a settled thread, and for a new one.
  run: ThreadRunState | null;
  // How full the context window was when the thread's last run ended.
  context: ContextUsage | null;
}

function newActive(): Active {
  const id = crypto.randomUUID();
  return { threadId: id, sessionId: id, initialMessages: [], run: null, context: null };
}

// useNavigation is where the user is: the open conversation, or a routine and
// its runs, kept in step with the address bar.
export function useNavigation({
  agentName,
  setAgentName,
  fullHistory,
  routinesEnabled,
  setError,
}: {
  agentName: string;
  setAgentName: (name: string) => void;
  fullHistory: boolean;
  routinesEnabled: boolean;
  setError: (message: string | null) => void;
}) {
  const [active, setActive] = useState<Active>(() => newActive());

  // What the address bar asked for when the page opened, read once at the
  // first render and then consumed.
  //
  // Not read from the URL where it is needed: the effects that keep the URL in
  // step with the app run before the agent list has arrived, so by then the
  // address bar says what we defaulted to rather than what was asked for.
  // Consumed rather than kept because it is for arriving at a URL, not for
  // following the user around afterwards — a thread left in here would be
  // reopened again the next time they switched agent.
  const openedThread = useRef(paramFromURL(THREAD_PARAM));
  const restoreRoutine = useRef(paramFromURL(ROUTINE_PARAM));
  const restoreRoutineThread = useRef(paramFromURL(THREAD_PARAM));
  // Bumped by every navigation, so a slow load for a place the user has
  // already left is dropped.
  const navigation = useRef(0);

  const [selectedRoutine, setSelectedRoutine] = useState<Routine | null>(null);
  const [routineThreads, setRoutineThreads] = useState<ThreadInfo[]>([]);
  const [routineHistoryError, setRoutineHistoryError] = useState("");
  const [routineHistoryLoading, setRoutineHistoryLoading] = useState(false);
  // Whether the open conversation is the selected routine's.
  const [routineThreadReady, setRoutineThreadReady] = useState(false);
  const routineHistoryRefresh = useRef<{ id: string; refresh: () => Promise<void> } | null>(null);

  const openThread = useCallback(
    async (threadId: string, targetAgent = agentName) => {
      const request = ++navigation.current;
      try {
        let { messages, run, context, nextCursor, sessionId } = await fetchThreadMessages({ agentName: targetAgent }, threadId);
        // Full-history mode sends the transcript back to a stateless backend.
        // Preserve that contract even though the history API is paginated.
        if (fullHistory) {
          while (nextCursor) {
            const page = await fetchThreadMessages({ agentName: targetAgent }, threadId, { cursor: nextCursor });
            messages = [...page.messages, ...messages];
            nextCursor = page.nextCursor;
          }
        }
        if (request !== navigation.current) return;
        setAgentName(targetAgent);
        setActive({ threadId, sessionId, initialMessages: messages, run, context, nextCursor });
        setRoutineThreadReady(true);
      } catch (e) {
        if (request !== navigation.current) return;
        setError(String(e));
        setRoutineHistoryError(String(e));
      }
    },
    [agentName, fullHistory, setAgentName, setError]
  );
  const openThreadRef = useRef(openThread);
  openThreadRef.current = openThread;

  // Leaving whatever is open: the address bar's requests are spent, and no
  // routine is selected.
  const leave = useCallback(() => {
    navigation.current++;
    restoreRoutine.current = "";
    openedThread.current = "";
    setSelectedRoutine(null);
  }, []);

  const selectThread = useCallback(
    async (t: ThreadInfo) => {
      leave();
      if (t.thread_id === active.threadId) return;
      await openThread(t.thread_id);
    },
    [leave, openThread, active.threadId]
  );

  const startNewChat = useCallback(() => {
    leave();
    setActive(newActive());
  }, [leave]);

  const changeAgent = useCallback((name: string) => {
    leave();
    setAgentName(name);
    setActive(newActive());
  }, [leave, setAgentName]);

  const selectRoutine = (routine: Routine) => {
    navigation.current++;
    restoreRoutine.current = "";
    openedThread.current = "";
    setRoutineThreadReady(false);
    setRoutineThreads([]);
    setRoutineHistoryError("");
    setRoutineHistoryLoading(true);
    // A fresh object also reopens the latest run when this routine is clicked again.
    setSelectedRoutine({ ...routine });
  };

  // Opens one of the selected routine's runs.
  const openRoutineThread = (thread: ThreadInfo) => {
    if (!selectedRoutine) return;
    setRoutineThreadReady(false);
    void openThread(thread.thread_id, thread.agent_name || selectedRoutine.agent);
  };

  // Rereads the selected routine's runs when one of them finishes.
  const routineRunFinished = useCallback((routineId: string) => {
    const history = routineHistoryRefresh.current;
    if (history?.id === routineId) void history.refresh();
  }, []);

  // Reopen whatever the address bar names, once there is an agent to open it
  // against. Runs on load and on an agent change; a thread already open is
  // left alone so this cannot fight the sidebar.
  useEffect(() => {
    if (!agentName || restoreRoutine.current) return;
    const wanted = openedThread.current;
    openedThread.current = "";
    if (!wanted || wanted === active.threadId) return;
    void openThread(wanted);
    // active.threadId is deliberately not a dependency: this is for arriving
    // at a URL, not for following the user around after that.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agentName, openThread]);

  // And keep both pointing at whatever is open.
  useEffect(() => rememberParam(AGENT_PARAM, agentName), [agentName]);
  useEffect(() => rememberParam(THREAD_PARAM, active.threadId), [active.threadId]);

  // A selected routine lists its runs and opens the latest, or the one the
  // address bar named, then keeps the list current as runs finish.
  useEffect(() => {
    if (!selectedRoutine) return;
    let cancelled = false;
    let pending = false;
    let initial = true;
    let refreshAgain = false;
    const request = navigation.current;
    async function refresh() {
      if (cancelled) return;
      // A completion arriving during a fetch must trigger another read;
      // the in-flight response may predate the newly saved conversation.
      if (pending) { refreshAgain = true; return; }
      pending = true;
      try {
        const threads = await fetchRoutineThreads(selectedRoutine!.id);
        if (cancelled) return;
        setRoutineThreads(threads);
        setRoutineHistoryError("");
        if (initial && threads.length && request === navigation.current) {
          initial = false;
          const restored = restoreRoutine.current ? threads.find(t => t.thread_id === restoreRoutineThread.current) : undefined;
          restoreRoutine.current = "";
          openedThread.current = "";
          const latest = restored || threads[0];
          await openThreadRef.current(latest.thread_id, latest.agent_name || selectedRoutine!.agent);
        }
      } catch (err) { if (!cancelled) setRoutineHistoryError(String(err)); }
      finally {
        pending = false;
        if (!cancelled) {
          setRoutineHistoryLoading(false);
          if (refreshAgain) { refreshAgain = false; void refresh(); }
        }
      }
    }
    routineHistoryRefresh.current = { id: selectedRoutine.id, refresh };
    void refresh();
    return () => { cancelled = true; routineHistoryRefresh.current = null; };
  }, [selectedRoutine]);

  // The routine the address bar names, once routines are known to be supported.
  useEffect(() => {
    const id = restoreRoutine.current;
    if (!routinesEnabled || !id) return;
    let cancelled = false;
    const request = navigation.current;
    fetchRoutine(id).then(routine => {
      if (!cancelled && request === navigation.current) {
        setRoutineHistoryLoading(true);
        setSelectedRoutine(routine);
      }
    }).catch(() => { restoreRoutine.current = ""; });
    return () => { cancelled = true; };
  }, [routinesEnabled]);
  useEffect(() => {
    if (!restoreRoutine.current) rememberParam(ROUTINE_PARAM, selectedRoutine?.id || "");
  }, [selectedRoutine, routineThreadReady]);

  return {
    active,
    selectThread,
    startNewChat,
    changeAgent,
    routine: {
      selected: selectedRoutine,
      threads: routineThreads,
      loading: routineHistoryLoading,
      error: routineHistoryError,
      // Whether the open conversation is one of the selected routine's runs.
      threadOpen: routineThreadReady,
      select: selectRoutine,
      openThread: openRoutineThread,
      runFinished: routineRunFinished,
    },
  };
}

import {
  HastekitEvent,
  readInterrupt,
  toResumeEntries,
  type BackgroundTask,
  type ContextUsage,
  type HastekitAgent,
  type ThreadRunState,
} from "@hastekit/copilotkit";
import { useEffect, useMemo, useState } from "react";
import type { Tray, TrayInterrupt } from "../components/ComposerTray";

// useThreadActivity follows what the open conversation is doing: a run that
// failed, context being compacted, tools still working in the background, and
// a pause waiting on the user. It combines what the page learned when it
// opened the thread (`run`, from the history endpoint) with what the agent's
// runs report since.
export function useThreadActivity({
  agent,
  agentName,
  threadId,
  run,
  context,
  onRunActivity,
}: {
  agent: HastekitAgent | null;
  agentName: string;
  threadId: string;
  // What the thread's last run left outstanding when it was opened.
  run: ThreadRunState | null;
  // How full the context window was when the thread was opened.
  context: ContextUsage | null;
  // Called when a run starts or ends, for anything that lists threads.
  onRunActivity: () => void;
}) {
  const [compacting, setCompacting] = useState(false);
  // How full the context window is: as the thread was opened, then as each
  // model call reports it.
  const [contextUsage, setContextUsage] = useState<ContextUsage | null>(null);
  useEffect(() => setContextUsage(context), [threadId, context]);
  // A failed run's message (server RUN_ERROR or a transport failure). CopilotKit
  // only logs these to the console, so the chat shows them as a banner.
  const [runError, setRunError] = useState<string | null>(null);

  // What the thread was left waiting on, as the page found it. Held apart from
  // `run` because it is transient: the moment a run starts, the run is the
  // source of truth and this is stale.
  const [restored, setRestored] = useState<ThreadRunState | null>(null);
  useEffect(() => setRestored(run), [threadId, run]);

  // Tasks seen starting in this session, keyed by task id.
  //
  // The reopened snapshot only covers a conversation the page has just
  // loaded. A task that starts while the user is sitting here is not in it —
  // the run that started the task ends normally, and without this the chat
  // simply goes quiet with nothing to say why. The run's own stream announces
  // both ends, so that is what this follows.
  const [liveTasks, setLiveTasks] = useState<Map<string, BackgroundTask>>(() => new Map());
  useEffect(() => setLiveTasks(new Map()), [threadId]);

  // The pause the running chat is showing, lifted out of the message list so
  // it can be drawn in the same place as a pause the page was reloaded into.
  const [liveInterrupt, setLiveInterrupt] = useState<TrayInterrupt | null>(null);
  useEffect(() => setLiveInterrupt(null), [threadId]);

  // Clear a stale run error when the user switches thread or agent. Restore
  // terminal failures when opening history after the replay log has expired.
  useEffect(
    () => setRunError(run?.status === "error" ? run.error || "The agent run failed." : null),
    [threadId, agentName, run]
  );

  useEffect(() => {
    setCompacting(false);
    if (!agent) return;
    const subscription = agent.subscribe({
      // A live run says what is happening, so the snapshot the page loaded
      // with is behind it and goes.
      //
      // On the RUN_STARTED event, not on initialization: CopilotChat connects
      // to the thread whenever it is given one, and connectAgent runs the same
      // path a real run does — so initialization fires even when there was
      // nothing to join. Clearing there wiped a restored approval card the
      // instant it was drawn. This fires only when a run is actually
      // streaming, which is the thing that supersedes it.
      onRunStartedEvent: () => {
        setRunError(null);
        setRestored(null);
        onRunActivity();
      },
      onCustomEvent: ({ event }) => {
        const value = (event.value ?? {}) as Record<string, string | undefined>;
        if (event.name === HastekitEvent.SummarizationStarted) { setCompacting(true); return; }
        if (event.name === HastekitEvent.SummarizationCompleted) { setCompacting(false); return; }
        if (event.name === HastekitEvent.ContextUsage) {
          const usage = event.value as ContextUsage | undefined;
          if (typeof usage?.tokens === "number") setContextUsage(usage);
          return;
        }
        if (event.name === HastekitEvent.BackgroundTaskStarted && value.taskId) {
          const task: BackgroundTask = {
            taskId: value.taskId,
            callId: value.toolCallId,
            toolName: value.toolName,
            streamId: value.streamId,
          };
          setLiveTasks((current) => new Map(current).set(task.taskId, task));
          return;
        }
        if (event.name === HastekitEvent.BackgroundTaskCompleted && value.taskId) {
          const taskId = value.taskId;
          setLiveTasks((current) => {
            if (!current.has(taskId)) return current;
            const next = new Map(current);
            next.delete(taskId);
            return next;
          });
          // The snapshot the page opened with may also be carrying it.
          setRestored((current) => {
            if (!current?.backgroundTasks?.length) return current;
            return { ...current, backgroundTasks: current.backgroundTasks.filter((t) => t.taskId !== taskId) };
          });
        }
      },
      onRunFinalized: () => { setCompacting(false); onRunActivity(); },
      onRunErrorEvent: ({ event }) => {
        setCompacting(false);
        setRunError(event.message || "The agent run failed.");
      },
      onRunFailed: ({ error }) => {
        setCompacting(false);
        setRunError(error?.message || String(error) || "The agent run failed.");
      },
    });
    return () => subscription.unsubscribe();
  }, [agent, onRunActivity]);

  // What the conversation is waiting on, however we came to know: the
  // snapshot it was opened with, plus anything seen starting since.
  const runningTasks = useMemo(() => {
    const byID = new Map<string, BackgroundTask>();
    for (const task of restored?.backgroundTasks ?? []) byID.set(task.taskId, task);
    for (const [id, task] of liveTasks) byID.set(id, task);
    return [...byID.values()];
  }, [restored, liveTasks]);

  // One tray, whichever way the pause reached us. A live one wins: it is the
  // run talking, and the snapshot the page loaded with is behind it.
  const tray = useMemo<Tray>(() => {
    if (liveInterrupt) return { tasks: runningTasks, interrupt: liveInterrupt, contextUsage };

    const waiting = (restored?.interrupts ?? []).map(readInterrupt);
    if (waiting.length) {
      return {
        tasks: runningTasks,
        contextUsage,
        interrupt: {
          key: "restored",
          interrupts: waiting,
          onSubmit: (decisions) => {
            // Cleared first: the resume starts a run, and the run is what
            // shows what happened next.
            setRestored(null);
            agent?.resume(toResumeEntries(waiting, decisions)).catch((e) => setRunError(String(e)));
          },
        },
      };
    }

    return { tasks: runningTasks, interrupt: null, contextUsage };
  }, [liveInterrupt, restored, runningTasks, agent, contextUsage]);

  return {
    tray,
    compacting,
    runError,
    dismissRunError: () => setRunError(null),
    // For InterruptHandler: publishes the live pause into the tray.
    publishInterrupt: setLiveInterrupt,
  };
}

import { watchRuns, type RunFeedEvent } from "@hastekit/copilotkit";
import { useEffect, useMemo, useRef, useState } from "react";

// How long the run feed holds a request open, and how long to wait after a
// failure before asking again. The wait sits comfortably inside the idle
// timeouts proxies usually impose.
const FEED_WAIT_SECONDS = 25;
const FEED_RETRY_MS = 2000;

// useRunFeed watches every conversation in the namespace, not just the open
// one, and tracks which have done something the user has not seen.
//
// The open conversation has its own stream. This covers the rest: a background
// task finishing in conversation A while the user reads conversation B, or a
// conversation that did not exist when the page loaded. Neither could be
// reached by anything keyed to a thread.
//
// The cursor is what makes a run that started and ended while the tab was in
// the background still count — the feed replays it on the next poll rather
// than dropping it.
//
// A thread is unseen until it is opened, so the badge means "there is
// something here you have not seen", not "this ran recently". The open
// conversation is being read as it happens; badging it would only ask the user
// to look at what they are already looking at.
export function useRunFeed(
  agentName: string,
  activeThreadId: string,
  // Called with every batch of events. May change on every render.
  onEvents: (events: RunFeedEvent[]) => void,
) {
  const [unseen, setUnseen] = useState<Set<string>>(() => new Set());
  const [threadGroups, setThreadGroups] = useState<Map<string, string>>(() => new Map());

  // The loop outlives any one thread and any one callback — that is the point
  // of it — so it reads both through refs rather than restarting.
  const activeThreadIdRef = useRef(activeThreadId);
  useEffect(() => {
    activeThreadIdRef.current = activeThreadId;
  }, [activeThreadId]);
  const onEventsRef = useRef(onEvents);
  onEventsRef.current = onEvents;

  useEffect(() => {
    if (!agentName) return;

    let stopped = false;
    const controller = new AbortController();

    const loop = async () => {
      let cursor = "";
      while (!stopped) {
        try {
          const seen = await watchRuns(
            { agentName },
            { cursor, waitSeconds: FEED_WAIT_SECONDS, signal: controller.signal }
          );
          if (stopped) return;
          cursor = seen.cursor;

          if (seen.events.length === 0) continue;
          onEventsRef.current(seen.events);

          setThreadGroups(current => {
            const next = new Map(current);
            for (const event of seen.events) {
              if (event.groupId) next.set(event.threadId, event.groupId);
            }
            return next;
          });
          setUnseen((current) => {
            const next = new Set(current);
            let changed = false;
            for (const event of seen.events) {
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
  }, [agentName]);

  // Opening a conversation is what marks it seen.
  useEffect(() => {
    setUnseen((current) => {
      if (!current.has(activeThreadId)) return current;
      const next = new Set(current);
      next.delete(activeThreadId);
      return next;
    });
  }, [activeThreadId]);

  // Routines with an unseen run, for the sidebar's routine list.
  const unseenRoutines = useMemo(() => {
    const groups = new Set<string>();
    for (const threadId of unseen) {
      const group = threadGroups.get(threadId);
      if (group && group !== "default") groups.add(group);
    }
    return groups;
  }, [unseen, threadGroups]);

  return { unseen, unseenRoutines };
}

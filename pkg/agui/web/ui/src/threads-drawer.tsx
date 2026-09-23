import { createElement, useLayoutEffect, useRef, type MouseEvent, type ReactNode } from "react";
import {
  defineCopilotKitThreadsDrawer,
  type CopilotKitThreadsDrawer,
  type ThreadSelectedDetail,
} from "@copilotkit/web-components/threads-drawer";
import type { ThreadInfo } from "./api";

// The controlled view uses HasteKit's history API. CopilotKit's React wrapper
// instead owns persistence through its Intelligence thread store.
defineCopilotKitThreadsDrawer();

export function ThreadsDrawer({ threads, activeThreadId, error, onSelect, onNew, onRetry, children }: {
  threads: ThreadInfo[];
  activeThreadId: string;
  error: string | null;
  onSelect: (thread: ThreadInfo) => void;
  onNew: () => void;
  onRetry: () => void;
  children: ReactNode;
}) {
  const ref = useRef<CopilotKitThreadsDrawer>(null);
  useLayoutEffect(() => {
    const drawer = ref.current!;
    drawer.threads = threads.map(thread => ({
      id: thread.thread_id,
      name: thread.title || "Untitled",
      archived: false,
      createdAt: thread.created_at,
      updatedAt: thread.updated_at,
    }));
    drawer.activeThreadId = activeThreadId;
    drawer.error = error;
  }, [threads, activeThreadId, error]);

  useLayoutEffect(() => {
    const drawer = ref.current!;
    const select = (event: Event) => {
      const { threadId } = (event as CustomEvent<ThreadSelectedDetail>).detail;
      const thread = threads.find(item => item.thread_id === threadId);
      if (thread) { onSelect(thread); drawer.open = false; }
    };
    const create = () => { onNew(); drawer.open = false; };
    drawer.addEventListener("thread-selected", select);
    drawer.addEventListener("new-thread", create);
    drawer.addEventListener("retry", onRetry);
    return () => {
      drawer.removeEventListener("thread-selected", select);
      drawer.removeEventListener("new-thread", create);
      drawer.removeEventListener("retry", onRetry);
    };
  }, [threads, onSelect, onNew, onRetry]);

  // React 18 custom elements need property assignment and native event listeners.
  return createElement("copilotkit-threads-drawer", {
    ref, class: "hastekit-threads-drawer",
    // Slotted navigation (skills/routines) must dismiss the mobile overlay too.
    onClick: (event: MouseEvent<HTMLElement>) => {
      if (event.target instanceof Element && event.target.closest("[slot]") && event.target.closest("button")) {
        ref.current!.open = false;
      }
    },
    label: "Conversations", "recent-label": "Recents",
  }, children);
}

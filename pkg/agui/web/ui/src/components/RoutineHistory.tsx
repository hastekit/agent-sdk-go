import type { Routine, ThreadInfo } from "../api";
import { RunRoutineButton } from "../routine-library";

// RoutineHistory lists a routine's past runs beside its open conversation.
export function RoutineHistory({
  routine,
  threads,
  loading,
  error,
  activeThreadId,
  unseen,
  onManage,
  onOpen,
}: {
  routine: Routine;
  threads: ThreadInfo[];
  loading: boolean;
  error: string;
  // The conversation on screen, or empty while none of this routine's is.
  activeThreadId: string;
  unseen: Set<string>;
  onManage: () => void;
  onOpen: (thread: ThreadInfo) => void;
}) {
  return (
    <aside className="routine-history" aria-label="Routine conversation history">
      <RunRoutineButton key={routine.id} routine={routine} />
      <div className="routine-history-heading"><h2>Run history</h2><button className="icon-btn" aria-label="Manage routines" onClick={onManage}>⚙</button></div>
      <p className="hint">{routine.name}</p>
      {error && <p className="hint error" role="alert">{error}</p>}
      {loading && <p className="hint">Loading conversations…</p>}
      {!loading && !error && !threads.length && <p className="hint">No runs yet.</p>}
      {threads.map((thread, index) => <button key={`${thread.agent_name}:${thread.thread_id}`}
        className={"thread-item routine-history-item" + (thread.thread_id === activeThreadId ? " selected" : "") + (unseen.has(thread.thread_id) ? " unseen" : "")}
        aria-current={thread.thread_id === activeThreadId ? "true" : undefined}
        onClick={() => onOpen(thread)}>
        <span className="routine-history-title">
          <span>{new Date(thread.created_at).toLocaleString()}{index === 0 ? " · Latest" : ""}</span>
          {unseen.has(thread.thread_id) && <span className="thread-dot" aria-label="New activity" />}
        </span>
        <small>{thread.agent_name || routine.agent} · {thread.title || "Routine run"}</small>
      </button>)}
    </aside>
  );
}

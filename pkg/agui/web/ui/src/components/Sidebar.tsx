import { relativeTime, type Routine, type ThreadInfo } from "../api";
import { ThreadsDrawer } from "../threads-drawer";

// Where the logo lives at runtime. public/ is copied to the build output
// as-is, and BASE_URL keeps the reference relative so the UI still finds
// it when the Go server is mounted under a sub-path.
const LOGO = `${import.meta.env.BASE_URL}hastekit-logo.svg`;

export function Sidebar({
  threads,
  activeThreadId,
  unseen,
  unseenRoutines,
  onSelect,
  onNew,
  onManageSkills,
  onManageMCP,
  onManageRoutines,
  routines,
  routinesLoading,
  routineError,
  selectedRoutineId,
  onSelectRoutine,
  onRetry,
  listingSupported,
  error,
}: {
  threads: ThreadInfo[];
  activeThreadId: string;
  unseen: Set<string>;
  unseenRoutines: Set<string>;
  onSelect: (t: ThreadInfo) => void;
  onNew: () => void;
  onManageSkills?: () => void;
  onManageMCP?: () => void;
  onManageRoutines?: () => void;
  routines: Routine[];
  selectedRoutineId?: string;
  onSelectRoutine: (routine: Routine) => void;
  routinesLoading: boolean;
  routineError: string;
  onRetry: () => void;
  listingSupported: boolean;
  error: string | null;
}) {
  return (
    <ThreadsDrawer threads={listingSupported ? threads : []} activeThreadId={selectedRoutineId ? "" : activeThreadId}
      error={error} onSelect={onSelect} onNew={onNew} onRetry={onRetry}>
      <div slot="header" className="drawer-header">
        <span className="brand"><img className="logo" src={LOGO} alt="" />HasteKit</span>
        {onManageSkills && <button className="nav-item" onClick={onManageSkills}>Skill library</button>}
        {onManageMCP && <button className="nav-item" onClick={onManageMCP}>MCP servers</button>}
      </div>
      <span slot="empty">{listingSupported ? "No conversations yet — start a new chat." : "Conversation history is not available for this agent."}</span>
        {onManageRoutines && <section slot="footer" aria-label="Routines" className="sidebar-routines">
          <div className="section-label routine-section-heading">
            <span>Routines</span>
            <button className="icon-btn" onClick={onManageRoutines} aria-label="Manage routines" title="Create and manage routines">+</button>
          </div>
          {routineError && <div className="hint error" role="alert">{routineError}</div>}
          {routinesLoading && <div className="hint">Loading routines…</div>}
          {!routinesLoading && !routineError && routines.length === 0 && <div className="hint">No enabled routines.</div>}
          {routines.map(routine => {
            const hasUnseen = unseenRoutines.has(routine.id);
            return <button key={routine.id} className={"thread-item" + (selectedRoutineId === routine.id ? " selected" : "") + (hasUnseen ? " unseen" : "")} onClick={() => onSelectRoutine(routine)}
              title={`${routine.name} · ${routine.agent}${hasUnseen ? " · new activity" : ""}`}>
              <span className="thread-title">{routine.name}</span>
              {hasUnseen && <span className="thread-dot" aria-label="New activity" />}
            </button>;
          })}
        </section>}

      {threads.map(thread => <span key={thread.thread_id} slot={`row:${thread.thread_id}`}
        className={"drawer-thread-content" + (unseen.has(thread.thread_id) ? " unseen" : "")}
        title={`${thread.title || "Untitled"} · ${relativeTime(thread.updated_at)}`}>
        <span className="thread-title">{thread.title || "Untitled"}</span>
        {unseen.has(thread.thread_id) && <span className="thread-dot" aria-label="New activity" />}
      </span>)}
    </ThreadsDrawer>
  );
}

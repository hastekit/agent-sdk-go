import { useEffect, useRef, useState, type FormEvent } from "react";
import { createRoutine, runRoutineNow, fetchRoutineAgents, fetchRoutines, setRoutineEnabled, type Routine } from "./api";

export function RoutineLibrary({ initialAgent, onClose, onChanged }: { initialAgent: string; onClose: () => void; onChanged: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [routines, setRoutines] = useState<Routine[]>([]);
  const [agents, setAgents] = useState<string[]>([]);
  const [agent, setAgent] = useState(initialAgent);
  const [name, setName] = useState("");
  const [instruction, setInstruction] = useState("");
  const [kind, setKind] = useState("once");
  const [at, setAt] = useState("");
  const [cron, setCron] = useState("0 9 * * 1-5");
  const [timezone, setTimezone] = useState(() => Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [revision, setRevision] = useState(0);

  useEffect(() => { dialog.current?.showModal(); }, []);
  useEffect(() => {
    let cancelled = false;
    setLoading(true); setError("");
    Promise.all([fetchRoutines(), fetchRoutineAgents()]).then(([items, names]) => {
      if (cancelled) return;
      setRoutines(items); setAgents(names);
      setAgent(current => names.includes(current) ? current : names[0] || "");
    }).catch(err => { if (!cancelled) setError(String(err)); })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, [revision]);

  async function save(event: FormEvent) {
    event.preventDefault();
    setError(""); setNotice("");
    if (!name.trim() || !instruction.trim()) { setError("Enter a name and instruction."); return; }
    const date = new Date(at);
    if (kind === "once" && (!Number.isFinite(date.getTime()) || date.getTime() <= Date.now())) {
      setError("Choose a future date and time."); return;
    }
    if (kind === "cron" && cron.trim().split(/\s+/).length !== 5) {
      setError("Use a five-field cron expression: minute hour day-of-month month day-of-week."); return;
    }
    setBusy(true);
    try {
      const saved = await createRoutine({ name: name.trim(), agent, instruction: instruction.trim(),
        schedule: kind === "once" ? { at: date.toISOString() } : { cron: cron.trim(), timezone: timezone.trim() || "UTC" } });
      setRoutines(items => [...items, saved]);
      setName(""); setInstruction(""); setAt("");
      setNotice(`Created ${saved.name}.`);
      onChanged();
    } catch (err) { setError(String(err)); }
    finally { setBusy(false); }
  }
  async function toggle(routine: Routine) {
    setBusy(true); setError(""); setNotice("");
    try {
      const saved = await setRoutineEnabled(routine.id, !routine.enabled);
      setRoutines(items => items.map(item => item.id === saved.id ? saved : item));
      onChanged();
    } catch (err) { setError(String(err)); }
    finally { setBusy(false); }
  }

  return <dialog ref={dialog} className="skill-library routine-library" onCancel={onClose} aria-labelledby="routine-library-title">
    <header><div><h2 id="routine-library-title">Routines</h2><p>Schedule an agent to carry out an instruction.</p></div><button onClick={onClose}>Close</button></header>
    <form onSubmit={save}>
      <fieldset disabled={busy || loading || !agents.length} className="routine-form">
        <legend>Create a routine</legend>
        <label>Name<input required value={name} onChange={e => setName(e.target.value)} placeholder="Morning summary" /></label>
        <label>Agent<select required value={agent} onChange={e => setAgent(e.target.value)}>{agents.map(name => <option key={name}>{name}</option>)}</select></label>
        <label>Instruction<textarea required rows={3} value={instruction} onChange={e => setInstruction(e.target.value)} placeholder="What should the agent do?" /></label>
        <label>Schedule<select value={kind} onChange={e => setKind(e.target.value)}><option value="once">One time</option><option value="cron">Repeating (cron)</option></select></label>
        {kind === "once" ? <label>Date and time<input required type="datetime-local" value={at} onChange={e => setAt(e.target.value)} /><small>In your local time zone ({Intl.DateTimeFormat().resolvedOptions().timeZone}).</small></label> : <>
          <label>Cron expression<input required value={cron} onChange={e => setCron(e.target.value)} aria-describedby="cron-help" /><small id="cron-help">Minute · hour · day of month · month · day of week. Example: 0 9 * * 1-5 runs at 9 AM on weekdays.</small></label>
          <label>Time zone<input required value={timezone} onChange={e => setTimezone(e.target.value)} placeholder="Asia/Kolkata" /><small>Use an IANA time zone, such as Asia/Kolkata or UTC.</small></label>
        </>}
        <button type="submit">{busy ? "Saving…" : "Create routine"}</button>
      </fieldset>
    </form>
    {error && <p role="alert" className="skill-library-error">{error}</p>}
    {notice && <p role="status" className="skill-library-notice">{notice}</p>}
    {!loading && !agents.length && <p>No target agents are available.</p>}
    <section aria-label="Saved routines">
      <header><h3>Saved routines</h3><button disabled={busy || loading} onClick={() => setRevision(v => v + 1)}>Refresh</button></header>
      {loading ? <p role="status">Loading routines…</p> : !routines.length && <p>No routines yet.</p>}
      {routines.map(routine => <article key={routine.id}>
        <div className="skill-summary"><strong>{routine.name}</strong><p>{routine.instruction}</p><small>{routine.agent} · {routine.schedule.at ? new Date(routine.schedule.at).toLocaleString() : `${routine.schedule.cron} (${routine.schedule.timezone || "UTC"})`} · {routine.enabled ? "Enabled" : "Paused"}</small></div>
        <RunRoutineButton routine={routine} />
        <button disabled={busy || loading} onClick={() => void toggle(routine)} aria-label={`${routine.enabled ? "Pause" : "Resume"} ${routine.name}`}>{routine.enabled ? "Pause" : "Resume"}</button>
      </article>)}
    </section>
  </dialog>;
}

export function RunRoutineButton({ routine }: { routine: Routine }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [queued, setQueued] = useState(false);
  async function run() {
    if (busy) return;
    setBusy(true); setError(""); setQueued(false);
    try { await runRoutineNow(routine.id); setQueued(true); }
    catch (err) { setError(String(err)); }
    finally { setBusy(false); }
  }
  return <div className="routine-run-action">
    <button type="button" disabled={busy || !routine.enabled} onClick={() => void run()}
      title={!routine.enabled ? "Resume this routine to run it" : "Run now without changing the schedule"}
      aria-label={`Run ${routine.name} now`}>{busy ? "Queuing…" : "Run now"}</button>
    {queued && <small role="status">Run queued.</small>}
    {error && <small role="alert" className="error">{error}</small>}
  </div>;
}

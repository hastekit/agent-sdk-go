import { useCallback, useEffect, useState } from "react";
import { fetchRoutines, type Routine } from "../api";

// useRoutineList keeps the sidebar's enabled routines current while routines
// are supported.
export function useRoutineList(enabled: boolean) {
  const [routines, setRoutines] = useState<Routine[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [revision, setRevision] = useState(0);

  useEffect(() => {
    if (!enabled) return;
    let cancelled = false;
    let pending = false;
    async function refresh() {
      if (pending) return;
      pending = true;
      try {
        const all = await fetchRoutines();
        if (!cancelled) {
          setRoutines(all.filter(routine => routine.enabled));
          setError("");
        }
      } catch (err) {
        if (!cancelled) setError(String(err));
      } finally {
        pending = false;
        if (!cancelled) setLoading(false);
      }
    }
    void refresh();
    // Agent tools and other clients can change definitions too.
    const interval = window.setInterval(() => { if (!document.hidden) void refresh(); }, 30_000);
    window.addEventListener("focus", refresh);
    return () => {
      cancelled = true;
      window.clearInterval(interval);
      window.removeEventListener("focus", refresh);
    };
  }, [enabled, revision]);

  // Reloads the list after this page changes a routine.
  const reload = useCallback(() => setRevision(value => value + 1), []);

  return { routines, loading, error, reload };
}

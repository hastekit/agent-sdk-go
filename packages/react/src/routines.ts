import { delay } from "./stream.js";
import {
  useEffect,
  useMemo,
  useSyncExternalStore,
  useState,
  useCallback,
} from "react";
import type { Thread, ChatTransport } from "./types.js";

// Match the server's mutually exclusive one-time and recurring schedules.
export type RoutineSchedule =
  | { at: string; cron?: never; timezone?: never }
  | { cron: string; timezone?: string; at?: never };

// Keep writable routine definitions separate from server-owned metadata.
export interface RoutineDefinition {
  name: string;
  agent: string;
  instruction: string;
  schedule: RoutineSchedule;
}
export interface Routine extends RoutineDefinition {
  id: string;
  namespace: string;
  version: number;
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

// Describe scheduler state without conflating queued work with a completed run.
export interface RoutineRun {
  id: string;
  agent_run_id: string;
  manual?: boolean;
  scheduled_at: string;
  started_at: string;
  finished_at?: string;
  retry_at: string;
  attempts: number;
  status: string;
  error?: string;
}
export interface RoutineStatus {
  definition_version: number;
  next_run_at: string;
  paused: boolean;
  pending?: RoutineRun;
  last_run?: RoutineRun;
}

// Allow alternative transports to implement routines independently of chat streaming.
export interface RoutineTransport {
  list(signal?: AbortSignal): Promise<Routine[]>;
  agents(signal?: AbortSignal): Promise<string[]>;
  get(id: string, signal?: AbortSignal): Promise<Routine>;
  create(definition: RoutineDefinition, signal?: AbortSignal): Promise<Routine>;
  update(
    id: string,
    definition: RoutineDefinition,
    signal?: AbortSignal,
  ): Promise<Routine>;
  delete(id: string, signal?: AbortSignal): Promise<void>;
  setEnabled(
    id: string,
    enabled: boolean,
    signal?: AbortSignal,
  ): Promise<Routine>;
  runNow(id: string, signal?: AbortSignal): Promise<RoutineRun>;
  status(id: string, signal?: AbortSignal): Promise<RoutineStatus>;
  threads(id: string, signal?: AbortSignal): Promise<Thread[]>;
}

// Expose list and mutation state as a stable external store for concurrent React.
class RoutineStore {
  private state = {
    routines: [] as Routine[],
    agents: [] as string[],
    loading: false,
    busy: false,
    error: null as Error | null,
  };
  private listeners = new Set<() => void>();
  private abort?: AbortController;
  private revision = 0;
  private active = true;
  constructor(private transport?: RoutineTransport) {}
  getSnapshot = () => this.state;
  subscribe = (listener: () => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  // Publish only immutable snapshots to mounted consumers.
  private publish(patch: Partial<typeof this.state>) {
    if (!this.active) return;
    this.state = { ...this.state, ...patch };
    this.listeners.forEach((listener) => listener());
  }

  // Cancel reads on cleanup; dispatched writes are never automatically retried or cancelled.
  mount = () => {
    this.active = true;
    void this.refresh().catch(() => {});
    return () => {
      this.active = false;
      this.abort?.abort();
    };
  };

  // Superseded reads cannot overwrite a mutation or a newer refresh.
  refresh = async () => {
    const revision = ++this.revision;
    this.abort?.abort();
    const abort = new AbortController();
    this.abort = abort;
    this.publish({ loading: true, error: null });
    try {
      if (!this.transport)
        throw new Error("Routines are not supported by this transport");
      const [routines, agents] = await Promise.all([
        this.transport.list(abort.signal),
        this.transport.agents(abort.signal),
      ]);
      if (!abort.signal.aborted && revision === this.revision)
        this.publish({ routines, agents });
    } catch (error) {
      if (!abort.signal.aborted && revision === this.revision) {
        this.publish({
          error: error instanceof Error ? error : new Error(String(error)),
        });
        throw error;
      }
    } finally {
      if (!abort.signal.aborted && revision === this.revision)
        this.publish({ loading: false });
    }
  };

  // Serialize mutations and apply their returned values without a fallible follow-up fetch.
  private async mutate<T>(
    operation: (transport: RoutineTransport) => Promise<T>,
    apply: (value: T) => void,
  ): Promise<T> {
    if (this.state.busy)
      throw new Error("A routine operation is already in progress");
    if (!this.transport)
      throw new Error("Routines are not supported by this transport");
    this.revision++;
    this.abort?.abort();
    this.publish({ busy: true, loading: false, error: null });
    try {
      const value = await operation(this.transport);
      this.revision++;
      this.abort?.abort();
      apply(value);
      return value;
    } catch (error) {
      this.publish({
        error: error instanceof Error ? error : new Error(String(error)),
      });
      throw error;
    } finally {
      this.publish({ busy: false, loading: false });
    }
  }

  // Replace the server's canonical definition while preserving the list order.
  private saved = (routine: Routine) => {
    const exists = this.state.routines.some((item) => item.id === routine.id);
    this.publish({
      routines: exists
        ? this.state.routines.map((item) =>
            item.id === routine.id ? routine : item,
          )
        : [...this.state.routines, routine],
    });
  };

  // Create a definition and retain the server-generated identity.
  create = (definition: RoutineDefinition) =>
    this.mutate((api) => api.create(definition), this.saved);

  // Save only writable fields, leaving version and timestamps to the server.
  update = (id: string, definition: RoutineDefinition) =>
    this.mutate((api) => api.update(id, definition), this.saved);

  // Remove a definition from local state only after the server confirms deletion.
  delete = (id: string) =>
    this.mutate(
      (api) => api.delete(id),
      () =>
        this.publish({
          routines: this.state.routines.filter((item) => item.id !== id),
        }),
    );

  // Pause or resume without rewriting the schedule.
  setEnabled = (id: string, enabled: boolean) =>
    this.mutate((api) => api.setEnabled(id, enabled), this.saved);

  // Queue a manual occurrence without changing its recurring schedule.
  runNow = (id: string) =>
    this.mutate(
      (api) => api.runNow(id),
      () => {},
    );
}

// Keep routine management independent from whichever conversation is currently selected.
export function useRoutines({ transport }: { transport: ChatTransport }) {
  const store = useMemo(
    () => new RoutineStore(transport.routines),
    [transport],
  );
  const snapshot = useSyncExternalStore(
    store.subscribe,
    store.getSnapshot,
    store.getSnapshot,
  );
  useEffect(() => store.mount(), [store]);
  return {
    ...snapshot,
    refresh: store.refresh,
    create: store.create,
    update: store.update,
    delete: store.delete,
    setEnabled: store.setEnabled,
    runNow: store.runNow,
  };
}

// Load occurrence history and refresh it whenever the agent announces a routine run.
export function useRoutineThreads({
  transport,
  routineId,
  agent,
}: {
  transport: ChatTransport;
  routineId: string;
  agent: string;
}) {
  const [threads, setThreads] = useState<Thread[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<Error | null>(null);
  const [revision, setRevision] = useState(0);
  const refresh = useCallback(() => setRevision((value) => value + 1), []);

  // Scope history to its transport and routine so late responses cannot replace a new selection.
  useEffect(() => {
    setThreads([]);
  }, [transport, routineId]);

  // Cancel superseded history reads while retaining the visible list during refresh.
  useEffect(() => {
    const abort = new AbortController();
    setLoading(true);
    setError(null);
    const request = transport.routines
      ? transport.routines.threads(routineId, abort.signal)
      : Promise.reject(
          new Error("Routines are not supported by this transport"),
        );
    void request
      .then((items) => {
        if (!abort.signal.aborted) setThreads(items);
      })
      .catch((error) => {
        if (!abort.signal.aborted)
          setError(error instanceof Error ? error : new Error(String(error)));
      })
      .finally(() => {
        if (!abort.signal.aborted) setLoading(false);
      });
    return () => abort.abort();
  }, [transport, routineId, revision]);

  // Include historical agent attribution because a routine's target can change over time.
  const agents = JSON.stringify(
    [
      ...new Set([
        agent,
        ...threads
          .map((thread) => thread.agent_name)
          .filter((name): name is string => !!name),
      ]),
    ].sort(),
  );
  useEffect(() => {
    const abort = new AbortController();
    const names = JSON.parse(agents) as string[];

    // Prefer lifecycle notifications; poll only when the transport lacks that capability.
    async function watch(name: string) {
      let cursor = "";
      let supported = !!transport.watchRuns;
      while (!abort.signal.aborted) {
        try {
          if (supported) {
            const page = await transport.watchRuns!(name, cursor, abort.signal);
            if (abort.signal.aborted) return;
            supported = page.supported;
            cursor = page.cursor;
            if (page.events.some((event) => event.groupId === routineId))
              refresh();
            if (supported) {
              await delay(250, abort.signal);
              continue;
            }
          }

          // Older servers still update occurrence lists without requiring manual refresh.
          await delay(5000, abort.signal);
          if (!abort.signal.aborted) refresh();
        } catch {
          if (!abort.signal.aborted) {
            try {
              await delay(2000, abort.signal);
            } catch {
              return;
            }
          }
        }
      }
    }
    for (const name of names) void watch(name);
    return () => abort.abort();
  }, [transport, routineId, agents, refresh]);

  return { threads, loading, error, refresh };
}

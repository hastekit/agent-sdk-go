import { EventReducer, prependMessages } from "./reducer.js";
import { delay, StreamUnavailableError } from "./stream.js";
import type {
  ChatEvent,
  ChatOptions,
  ChatSnapshot,
  ContentPart,
  Message,
  RunInput,
} from "./types.js";

// Normalize thrown values before exposing failures to the UI.
function asError(value: unknown): Error {
  return value instanceof Error ? value : new Error(String(value));
}

// Create an isolated store for one agent and conversation group.
export class ChatController {
  private snapshot: ChatSnapshot = {
    skills: [],
    skillSelection: { enable: [], disable: [] },
    loadingSkills: false,
    skillsError: null,
    threads: [],
    threadsSupported: true,
    loadingThreads: false,
    threadId: null,
    sessionId: null,
    messages: [],
    loadingMessages: false,
    loadingOlder: false,
    hasOlderMessages: false,
    connection: "idle",
    isRunning: false,
    isStopping: false,
    isCompacting: false,
    run: null,
    state: {},
    activeThreadIds: [],
    error: null,
    lastEvent: null,
  };
  private readonly listeners = new Set<() => void>();
  private generation = 0;
  private listGeneration = 0;
  private cursor = "";
  private streamId?: string;
  private selectionAbort = new AbortController();
  private skillsAbort?: AbortController;
  private skillDefaults = new Map<string, boolean>();
  private listAbort?: AbortController;
  private streamAbort?: AbortController;
  private feedAbort?: AbortController;
  private initialized = false;
  private mounted = false;

  constructor(private readonly options: ChatOptions) {}

  // React reads a stable snapshot reference until the store publishes a change.
  getSnapshot = (): ChatSnapshot => this.snapshot;

  // Subscribers own their listener lifetime independently of network connections.
  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  // Publish immutable state so useSyncExternalStore can detect changes.
  private update(patch: Partial<ChatSnapshot>): void {
    this.snapshot = { ...this.snapshot, ...patch };
    this.listeners.forEach((listener) => listener());
  }

  // Generate provider-compatible identities without depending on a browser window.
  private id(): string {
    return this.options.createId?.() ?? globalThis.crypto.randomUUID();
  }

  // Start automatic loading and observation; cleanup detaches without stopping server work.
  mount = (): (() => void) => {
    this.mounted = true;
    void this.refreshThreads().catch(() => {});
    void this.refreshSkills().catch(() => {});
    // Restore the initial selection once, including effect replay in development mode.
    if (!this.initialized) {
      this.initialized = true;
      if (this.options.initialThreadId)
        void this.selectThread(this.options.initialThreadId).catch(() => {});
    } else if (this.snapshot.threadId) {
      void this.selectThread(this.snapshot.threadId).catch(() => {});
    }
    // Track runs created by other clients while this consumer is mounted.
    this.startFeed();

    // Detach every request without issuing a server-side stop.
    return () => {
      this.mounted = false;
      this.feedAbort?.abort();
      this.listAbort?.abort();
      this.skillsAbort?.abort();
      this.resetSelection();
    };
  };

  // Invalidate asynchronous work before replacing the visible conversation.
  private resetSelection(): number {
    this.generation++;
    this.selectionAbort.abort();
    this.selectionAbort = new AbortController();
    this.streamAbort?.abort();
    this.streamAbort = undefined;
    this.streamId = undefined;
    this.cursor = "";
    return this.generation;
  }

  // Clear a surfaced error after the application acknowledges it.
  clearError = (): void => {
    this.update({ error: null });
  };

  // Reload sidebar rows, ignoring superseded requests even if a transport ignores abort.
  refreshThreads = async (): Promise<void> => {
    const generation = ++this.listGeneration;
    this.listAbort?.abort();
    const abort = new AbortController();
    this.listAbort = abort;
    this.update({ loadingThreads: true });
    try {
      const result = await this.options.transport.listThreads(
        this.options.agent,
        this.options.groupId ?? "default",
        abort.signal,
      );
      if (!abort.signal.aborted && generation === this.listGeneration) {
        this.update({
          threads: result.threads,
          threadsSupported: result.supported,
        });
      }
    } catch (error) {
      if (!abort.signal.aborted && generation === this.listGeneration) {
        this.update({ error: asError(error) });
        throw error;
      }
    } finally {
      if (!abort.signal.aborted && generation === this.listGeneration)
        this.update({ loadingThreads: false });
    }
  };

  // Refresh the catalog independently of chat errors and discard stale responses.
  refreshSkills = async (): Promise<void> => {
    this.skillsAbort?.abort();
    const abort = new AbortController();
    this.skillsAbort = abort;
    this.update({ loadingSkills: true, skillsError: null });
    try {
      const catalog =
        (await this.options.transport.listSkills?.(
          this.options.agent,
          abort.signal,
        )) ?? [];
      if (abort.signal.aborted) return;

      // Retain choices only for skills that remain available and optional.
      this.skillDefaults = new Map(
        catalog.map((skill) => [skill.name, skill.enabled]),
      );
      const optional = new Set(
        catalog.filter((skill) => !skill.required).map((skill) => skill.name),
      );
      const selection = this.snapshot.skillSelection;
      const skillSelection = {
        enable: selection.enable.filter((name) => optional.has(name)),
        disable: selection.disable.filter((name) => optional.has(name)),
      };
      const skills = catalog.map((skill) => ({
        ...skill,
        enabled:
          !!skill.required ||
          (!skillSelection.disable.includes(skill.name) &&
            (skill.enabled || skillSelection.enable.includes(skill.name))),
      }));
      this.update({ skills, skillSelection });
    } catch (error) {
      if (!abort.signal.aborted) {
        this.update({ skillsError: asError(error) });
        throw error;
      }
    } finally {
      if (!abort.signal.aborted) this.update({ loadingSkills: false });
    }
  };

  // Apply composer choices to subsequent runs while preserving required skills.
  setSkillEnabled = (name: string, enabled: boolean): void => {
    const skill = this.snapshot.skills.find((item) => item.name === name);
    if (!skill) throw new Error(`Unknown skill: ${name}`);
    if (skill.required && !enabled)
      throw new Error(`Skill is required: ${name}`);

    // Store only deviations from the catalog defaults.
    const enable = this.snapshot.skillSelection.enable.filter(
      (item) => item !== name,
    );
    const disable = this.snapshot.skillSelection.disable.filter(
      (item) => item !== name,
    );
    if (!skill.required && enabled !== this.skillDefaults.get(name))
      (enabled ? enable : disable).push(name);
    this.update({
      skillSelection: { enable, disable },
      skills: this.snapshot.skills.map((item) =>
        item.name === name ? { ...item, enabled } : item,
      ),
    });
  };

  // Restore the server defaults without changing the selected conversation.
  resetSkills = (): void => {
    this.update({
      skillSelection: { enable: [], disable: [] },
      skills: this.snapshot.skills.map((skill) => ({
        ...skill,
        enabled: !!skill.required || !!this.skillDefaults.get(skill.name),
      })),
    });
  };

  // Allocate a local draft; the backend creates its persisted conversation on send.
  newThread = (): string => {
    this.resetSelection();
    const id = this.id();
    this.update({
      threadId: id,
      sessionId: id,
      messages: [],
      state: {},
      run: null,
      loadingMessages: false,
      loadingOlder: false,
      hasOlderMessages: false,
      connection: "idle",
      isRunning: false,
      isStopping: false,
      isCompacting: false,
      error: null,
      lastEvent: null,
    });
    return id;
  };

  // Restore the newest history page, then join any available run without posting a turn.
  selectThread = async (threadId: string): Promise<void> => {
    const generation = this.resetSelection();
    const signal = this.selectionAbort.signal;
    this.update({
      threadId,
      sessionId: threadId,
      messages: [],
      state: {},
      run: null,
      loadingMessages: true,
      loadingOlder: false,
      hasOlderMessages: false,
      connection: "idle",
      isRunning: false,
      isStopping: false,
      isCompacting: false,
      error: null,
      lastEvent: null,
    });
    try {
      const page = await this.options.transport.loadMessages(
        this.options.agent,
        threadId,
        "",
        signal,
      );
      if (generation !== this.generation || signal.aborted) return;
      this.cursor = page.nextCursor;
      this.update({
        messages: page.messages,
        run: page.run,
        sessionId: page.sessionId,
        hasOlderMessages: Boolean(this.cursor),
        loadingMessages: false,
      });
      void this.connect().catch(() => {});
    } catch (error) {
      if (generation !== this.generation || signal.aborted) return;
      this.update({ error: asError(error), loadingMessages: false });
      throw error;
    }
  };

  // Prepend older history while preserving messages received during the request.
  loadOlderMessages = async (): Promise<void> => {
    const threadId = this.snapshot.threadId;
    if (!threadId || !this.cursor || this.snapshot.loadingOlder) return;
    const generation = this.generation;
    const signal = this.selectionAbort.signal;
    this.update({ loadingOlder: true });
    try {
      const page = await this.options.transport.loadMessages(
        this.options.agent,
        threadId,
        this.cursor,
        signal,
      );
      if (generation !== this.generation || signal.aborted) return;
      this.cursor = page.nextCursor;
      this.update({
        messages: prependMessages(page.messages, this.snapshot.messages),
        hasOlderMessages: Boolean(this.cursor),
      });
    } catch (error) {
      if (generation === this.generation && !signal.aborted) {
        this.update({ error: asError(error) });
        throw error;
      }
    } finally {
      if (generation === this.generation && !signal.aborted)
        this.update({ loadingOlder: false });
    }
  };

  // Build the AG-UI request without embedding application-specific UI state.
  private input(
    messages: Message[],
    forwardedProps: Record<string, unknown> = {},
  ): RunInput {
    return {
      threadId: this.snapshot.threadId!,
      runId: this.id(),
      messages,
      state: this.snapshot.state,
      tools: [],
      context: this.options.context ?? [],
      forwardedProps: {
        ...this.options.forwardedProps,
        ...(this.snapshot.skillSelection.enable.length ||
        this.snapshot.skillSelection.disable.length
          ? {
              skills: {
                enable: [...this.snapshot.skillSelection.enable],
                disable: [...this.snapshot.skillSelection.disable],
              },
            }
          : {}),
        ...forwardedProps,
      },
    };
  }

  // Send one user turn, with optimistic display and no implicit retry of the POST.
  sendMessage = async (
    content: string | ContentPart[],
    forwardedProps?: Record<string, unknown>,
  ): Promise<void> => {
    if (this.streamAbort || this.snapshot.loadingMessages)
      throw new Error(
        "Wait for the current run or history load before sending",
      );
    if (
      (typeof content === "string" && !content.trim()) ||
      (Array.isArray(content) && content.length === 0)
    )
      throw new Error("Message content is required");
    if (!this.snapshot.threadId) this.newThread();
    const message: Message = { id: `msg_${this.id()}`, role: "user", content };
    this.update({
      messages: [...this.snapshot.messages, message],
      error: null,
      run: null,
    });
    const messages = this.options.fullHistory
      ? this.snapshot.messages
      : [message];
    await this.consume(this.input(messages, forwardedProps));
  };

  // Resume an approval without adding a synthetic user message to the transcript.
  resume = async (
    decisions: { toolCallId: string; approved: boolean; content?: unknown }[],
  ): Promise<void> => {
    if (!this.snapshot.threadId)
      throw new Error("Select a conversation before resuming");
    if (this.streamAbort || this.snapshot.loadingMessages)
      throw new Error(
        "Wait for the current stream or history load before resuming",
      );
    if (!decisions.length)
      throw new Error("At least one approval decision is required");
    this.update({ run: null });
    await this.consume(this.input([], { command: { resume: { decisions } } }));
  };

  // Attach once to the selected conversation's current stream.
  connect = async (): Promise<void> => {
    if (
      !this.snapshot.threadId ||
      this.snapshot.loadingMessages ||
      this.streamAbort
    )
      return;
    await this.consume();
  };

  // Disconnecting changes browser observation only and never cancels the server run.
  disconnect = (): void => {
    this.streamAbort?.abort();
    this.streamAbort = undefined;
    this.update({ connection: "idle", isRunning: false, isStopping: false });
  };

  // Rebuild the transcript from persisted history and a fresh replay when requested.
  reconnect = async (): Promise<void> => {
    if (this.snapshot.threadId) await this.selectThread(this.snapshot.threadId);
  };

  // Send a server cancellation request while continuing to read its terminal event.
  stop = async (): Promise<void> => {
    const thread = this.snapshot.threadId;
    if (!thread || this.snapshot.isStopping) return;
    const generation = this.generation;
    this.update({ isStopping: true });
    try {
      await this.options.transport.stop(
        this.options.agent,
        thread,
        this.streamId,
        this.selectionAbort.signal,
      );
    } catch (error) {
      if (generation === this.generation) {
        this.update({ error: asError(error) });
        throw error;
      }
    } finally {
      if (generation === this.generation) this.update({ isStopping: false });
    }
  };

  // Delegate attachment storage using the session restored from history.
  uploadAttachment = async (file: File) => {
    if (!this.options.transport.uploadAttachment)
      throw new Error("Attachments are not supported by this transport");
    if (!this.snapshot.threadId) this.newThread();
    return this.options.transport.uploadAttachment(
      file,
      this.snapshot.sessionId!,
      this.selectionAbort.signal,
    );
  };

  // Consume a single run pipeline so reconnects retain partially assembled messages.
  private async consume(input?: RunInput): Promise<void> {
    const thread = this.snapshot.threadId!;
    const generation = this.generation;
    const abort = new AbortController();
    this.streamAbort = abort;
    this.streamId = undefined;
    const reducer = new EventReducer();

    // Wait for publication when the feed has already announced a running thread.
    const waitForRun = !input && this.snapshot.activeThreadIds.includes(thread);
    let sawEvent = false;
    let endedNormally = false;
    const current = () =>
      generation === this.generation &&
      this.streamAbort === abort &&
      !abort.signal.aborted;
    this.update({
      connection: "connecting",
      isRunning: Boolean(input),
      error: null,
    });
    try {
      const events = this.options.transport.stream(
        this.options.agent,
        thread,
        input,
        {
          signal: abort.signal,
          waitForRun,
          onStatus: (connection) => {
            if (current()) this.update({ connection });
          },
        },
      );
      for await (const event of events) {
        if (!current()) return;
        sawEvent = true;
        this.applyEvent(reducer, event);
      }
      endedNormally = true;
    } catch (error) {
      if (!current()) return;
      if (error instanceof StreamUnavailableError) {
        // Recovery errors must be visible even when this stream was attached automatically.
        try {
          const page = await this.options.transport.loadMessages(
            this.options.agent,
            thread,
            "",
            abort.signal,
          );
          if (!current()) return;
          this.cursor = page.nextCursor;
          this.update({
            messages: prependMessages(this.snapshot.messages, page.messages),
            run: page.run,
            sessionId: page.sessionId,
            hasOlderMessages: Boolean(this.cursor),
          });
        } catch (recoveryError) {
          if (!current()) return;
          this.update({ error: asError(recoveryError) });
          throw recoveryError;
        }
      } else {
        this.update({ error: asError(error) });
        throw error;
      }
    } finally {
      if (current()) {
        this.streamAbort = undefined;
        this.streamId = undefined;
        this.update({
          connection: "idle",
          isRunning: false,
          isStopping: false,
          isCompacting: false,
        });
        void this.refreshThreads().catch(() => {});

        // A feed notice arriving during an empty GET deserves one publication-aware join.
        if (
          endedNormally &&
          !input &&
          !waitForRun &&
          !sawEvent &&
          this.snapshot.activeThreadIds.includes(thread)
        ) {
          void this.connect().catch(() => {});
        }
      }
    }
  }

  // Apply protocol state before notifying application observers.
  private applyEvent(reducer: EventReducer, event: ChatEvent): void {
    if (event.type === "CUSTOM" && event.name === "hastekit.stream_id") {
      this.streamId = (event.value as { streamId?: string })?.streamId;
    }
    this.update({
      ...reducer.apply(this.snapshot, event),
      lastEvent: event,
      connection: "streaming",
      isRunning: true,
    });
    if (event.type === "RUN_STARTED")
      void this.refreshThreads().catch(() => {});
    this.options.onEvent?.(event);
  }

  // Watch other tabs and background runs without starting duplicate local streams.
  private startFeed(): void {
    if (this.options.watchRuns === false || !this.options.transport.watchRuns)
      return;
    this.feedAbort?.abort();
    const abort = new AbortController();
    this.feedAbort = abort;
    void (async () => {
      let cursor = "";
      while (this.mounted && !abort.signal.aborted) {
        try {
          const page = await this.options.transport.watchRuns!(
            this.options.agent,
            cursor,
            abort.signal,
          );
          if (abort.signal.aborted || !page.supported) return;
          cursor = page.cursor;
          if (page.events.length) {
            const active = new Set(this.snapshot.activeThreadIds);
            for (const event of page.events) {
              if (
                event.groupId &&
                event.groupId !== (this.options.groupId ?? "default")
              )
                continue;
              if (event.event === "RUN_STARTED") active.add(event.threadId);
              else active.delete(event.threadId);
              if (
                event.threadId === this.snapshot.threadId &&
                !this.streamAbort &&
                !this.snapshot.loadingMessages
              ) {
                void this.selectThread(event.threadId).catch(() => {});
              }
            }
            this.update({ activeThreadIds: [...active] });
            void this.refreshThreads().catch(() => {});
          }
          // Avoid spinning when a proxy or custom transport answers immediately.
          await delay(250, abort.signal);
        } catch {
          if (!abort.signal.aborted) await delay(2_000, abort.signal);
        }
      }
    })();
  }
}

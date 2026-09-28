import { EventReducer, prependMessages } from "./reducer.js";
import { delay, StreamUnavailableError } from "./stream.js";
import type {
  ChatEvent,
  ChatOptions,
  ChatSnapshot,
  ClientTool,
  ContentPart,
  MCPServerConfig,
  Message,
  ResumeEntry,
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
    skillSelection: { disable: [] },
    loadingSkills: false,
    skillsError: null,
    mcpServers: [],
    mcpSelection: { disable: [] },
    loadingMCPServers: false,
    mcpServersError: null,
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
  private mcpAbort?: AbortController;
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
    void this.refreshMCPServers().catch(() => {});
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
      this.mcpAbort?.abort();
      this.resetSelection();
    };
  };

  // Client tool results for the selected conversation, by call id: a handler
  // runs once per call, however many runs or rejoins see the call.
  private clientToolResults = new Map<string, Promise<ToolResult>>();
  // Calls whose results have already resumed the conversation. A call is
  // resumed once: a run that pauses on it again is not answered in a loop.
  private resumedClientCalls = new Set<string>();

  // Invalidate asynchronous work before replacing the visible conversation.
  private resetSelection(): number {
    this.generation++;
    this.clientToolResults = new Map();
    this.resumedClientCalls = new Set();
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
        (await this.options.transport.listSkills?.(abort.signal)) ?? [];
      if (abort.signal.aborted) return;

      // Retain choices only for skills that still exist.
      const available = new Set(catalog.map((skill) => skill.name));
      const skillSelection = {
        disable: this.snapshot.skillSelection.disable.filter((name) =>
          available.has(name),
        ),
      };
      const skills = catalog.map((skill) => ({
        ...skill,
        enabled: !skillSelection.disable.includes(skill.name),
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

  // Apply composer choices to subsequent runs. Agents' global skills are not
  // listed here and are always on.
  setSkillEnabled = (name: string, enabled: boolean): void => {
    const skill = this.snapshot.skills.find((item) => item.name === name);
    if (!skill) throw new Error(`Unknown skill: ${name}`);

    // Every skill starts enabled, so the selection only records disabled ones.
    const disable = this.snapshot.skillSelection.disable.filter(
      (item) => item !== name,
    );
    if (!enabled) disable.push(name);
    this.update({
      skillSelection: { disable },
      skills: this.snapshot.skills.map((item) =>
        item.name === name ? { ...item, enabled } : item,
      ),
    });
  };

  // Turn every skill back on without changing the selected conversation.
  resetSkills = (): void => {
    this.update({
      skillSelection: { disable: [] },
      skills: this.snapshot.skills.map((skill) => ({
        ...skill,
        enabled: true,
      })),
    });
  };

  // Refresh MCP servers independently of chat errors and discard stale responses.
  refreshMCPServers = async (): Promise<void> => {
    this.mcpAbort?.abort();
    const abort = new AbortController();
    this.mcpAbort = abort;
    this.update({ loadingMCPServers: true, mcpServersError: null });
    try {
      const listed =
        (await this.options.transport.mcp?.list(abort.signal)) ?? [];
      if (abort.signal.aborted) return;

      // Only the user's own servers can be turned off; drop choices for the rest.
      const optional = new Set(
        listed
          .filter((server) => !!server.namespace)
          .map((server) => server.name),
      );
      const mcpSelection = {
        disable: this.snapshot.mcpSelection.disable.filter((name) =>
          optional.has(name),
        ),
      };
      const mcpServers = listed.map((server) => ({
        ...server,
        enabled:
          !optional.has(server.name) ||
          !mcpSelection.disable.includes(server.name),
      }));
      this.update({ mcpServers, mcpSelection });
    } catch (error) {
      if (!abort.signal.aborted) {
        this.update({ mcpServersError: asError(error) });
        throw error;
      }
    } finally {
      if (!abort.signal.aborted) this.update({ loadingMCPServers: false });
    }
  };

  // Apply composer choices to subsequent runs; global servers cannot be turned off.
  setMCPServerEnabled = (name: string, enabled: boolean): void => {
    const server = this.snapshot.mcpServers.find((item) => item.name === name);
    if (!server) throw new Error(`Unknown MCP server: ${name}`);
    if (!server.namespace && !enabled)
      throw new Error(`MCP server is global and always enabled: ${name}`);
    const disable = this.snapshot.mcpSelection.disable.filter(
      (item) => item !== name,
    );
    if (!enabled) disable.push(name);
    this.update({
      mcpSelection: { disable },
      mcpServers: this.snapshot.mcpServers.map((item) =>
        item.name === name ? { ...item, enabled } : item,
      ),
    });
  };

  // Turn every MCP server back on without changing the selected conversation.
  resetMCPServers = (): void => {
    this.update({
      mcpSelection: { disable: [] },
      mcpServers: this.snapshot.mcpServers.map((server) => ({
        ...server,
        enabled: true,
      })),
    });
  };

  // Add or replace one of the user's servers, then reload the list.
  saveMCPServer = async (
    name: string,
    config: MCPServerConfig,
  ): Promise<void> => {
    if (!this.options.transport.mcp)
      throw new Error("MCP server management is not supported");
    await this.options.transport.mcp.save(name, config);
    await this.refreshMCPServers();
  };

  // Remove one of the user's servers and its OAuth grant, then reload the list.
  removeMCPServer = async (name: string): Promise<void> => {
    if (!this.options.transport.mcp)
      throw new Error("MCP server management is not supported");
    await this.options.transport.mcp.remove(name);
    await this.refreshMCPServers();
  };

  // URLs for OAuth servers: open connectUrl in a new tab; register callbackUrl with the provider.
  mcpConnectUrl = (name: string): string | undefined =>
    this.options.transport.mcp?.connectUrl(name);
  mcpCallbackUrl = (name: string): string | undefined =>
    this.options.transport.mcp?.callbackUrl(name);

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
        error:
          page.run?.status === "error"
            ? new Error(page.run.error || "Agent run failed")
            : null,
        sessionId: page.sessionId,
        hasOlderMessages: Boolean(this.cursor),
        loadingMessages: false,
      });
      void this.connect().catch(() => {});
      // A thread left waiting on this client's tools — by a page that went
      // away before answering — is answered now rather than staying paused.
      if (page.run?.pendingToolCallIds?.length)
        void this.answerClientTools(page.run.pendingToolCallIds).catch(() => {});
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
      tools: (this.options.clientTools ?? []).map((tool) => ({
        name: tool.name,
        description: tool.description ?? "",
        parameters: tool.parameters ?? { type: "object", properties: {} },
      })),
      context: this.options.context ?? [],
      forwardedProps: {
        ...this.options.forwardedProps,
        ...(this.snapshot.skillSelection.disable.length
          ? {
              skills: {
                disable: [...this.snapshot.skillSelection.disable],
              },
            }
          : {}),
        ...(this.snapshot.mcpSelection.disable.length
          ? { mcp: { disable: [...this.snapshot.mcpSelection.disable] } }
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

  // Answer the run's open interrupts (AG-UI 1.0 resume entries) without adding a
  // synthetic user message to the transcript. Answer every open interrupt at once.
  resume = async (entries: ResumeEntry[]): Promise<void> => {
    if (!this.snapshot.threadId)
      throw new Error("Select a conversation before resuming");
    if (this.streamAbort || this.snapshot.loadingMessages)
      throw new Error(
        "Wait for the current stream or history load before resuming",
      );
    if (!entries.length)
      throw new Error("At least one interrupt answer is required");
    this.update({ run: null });
    await this.consume({ ...this.input([]), resume: entries });
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

    // A run this controller started runs its tools as their calls stream in,
    // and sends each result at once: a run waiting for it takes it and carries
    // on. A run it joined — or one its turn folded into — may be replaying
    // calls already answered, so it only answers, when it ends, calls to this
    // client's tools still left unanswered.
    const clientTools = this.options.clientTools?.length
      ? new ClientToolRun(
          Boolean(input),
          (toolCallId, name, args) =>
            this.clientToolResult(thread, toolCallId, name, args),
          (result) => this.sendToolResults(thread, [result]),
        )
      : undefined;

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
      error: input ? null : this.snapshot.error,
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
          onFolded: () => clientTools?.joined(),
        },
      );
      for await (const event of events) {
        if (!current()) return;
        sawEvent = true;
        clientTools?.observe(event);
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
            error:
              page.run?.status === "error"
                ? new Error(page.run.error || "Agent run failed")
                : null,
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

    // A run paused only on this client's tools resumes with their results. If
    // a result sent early already resumed it, the thread is running and this
    // turn joins that run instead of starting another.
    const pausedOn = endedNormally ? clientTools?.pausedOn() : undefined;
    if (!pausedOn?.length) return;
    await this.resumeWith(thread, generation, await Promise.all(pausedOn));
  }

  // Answers client tool calls the conversation is paused on, from the calls in
  // its history. A handler runs once per call (see clientToolResults).
  private async answerClientTools(toolCallIds: string[]): Promise<void> {
    const thread = this.snapshot.threadId;
    if (!thread || !this.options.clientTools?.length) return;
    const generation = this.generation;
    const wanted = new Set(toolCallIds);
    const pending: Promise<ToolResult>[] = [];
    for (const message of this.snapshot.messages) {
      for (const call of message.toolCalls ?? []) {
        if (!wanted.has(call.id)) continue;
        const result = this.clientToolResult(
          thread,
          call.id,
          call.function.name,
          call.function.arguments,
        );
        if (result) pending.push(result);
      }
    }
    if (!pending.length) return;
    await this.resumeWith(thread, generation, await Promise.all(pending));
  }

  // Resumes a conversation with its tool results: as a run followed here when
  // nothing else is streaming, otherwise sent for the run that is to take them.
  private async resumeWith(
    thread: string,
    generation: number,
    results: ToolResult[],
  ): Promise<void> {
    if (generation === this.generation) {
      results = results.filter((r) => !this.resumedClientCalls.has(r.toolCallId));
      if (!results.length) return;
      for (const r of results) this.resumedClientCalls.add(r.toolCallId);
    }
    if (generation !== this.generation) {
      // The user moved on; the paused conversation still gets its answer.
      this.sendToolResults(thread, results);
      return;
    }
    if (this.streamAbort) {
      this.sendToolResults(thread, results);
      return;
    }
    await this.consume(this.toolResultsInput(thread, results));
  }

  // Runs this client's tool for a call once, or returns undefined when the tool
  // is not one of its own. Handlers see the conversation's signal, so leaving
  // the conversation can cancel their work; its results are still sent.
  private clientToolResult(
    threadId: string,
    toolCallId: string,
    name: string,
    argsText: string,
  ): Promise<ToolResult> | undefined {
    const cached = this.clientToolResults.get(toolCallId);
    if (cached) return cached;
    const tool = this.options.clientTools?.find((t) => t.name === name);
    if (!tool) return undefined;
    let args: unknown = {};
    try {
      args = JSON.parse(argsText || "{}");
    } catch {
      // Malformed arguments still reach the handler as an empty object.
    }
    const signal = this.selectionAbort.signal;
    // A failing tool still answers, so the model can react instead of the run hanging.
    const result = Promise.resolve()
      .then(() => tool.handler(args, { toolCallId, threadId, signal }))
      .then(
        toolResultContent,
        (error) =>
          `Error: ${error instanceof Error ? error.message : String(error)}`,
      )
      .then((content) => ({ toolCallId, content }));
    this.clientToolResults.set(toolCallId, result);
    return result;
  }

  // Sends tool results as a turn without following the run it may start. Not
  // tied to any stream: a result is worth delivering after the page moved on.
  private sendToolResults(thread: string, results: ToolResult[]): void {
    void this.options.transport
      .sendToolResults?.(
        this.options.agent,
        thread,
        this.toolResultsInput(thread, results),
      )
      .catch(() => {});
  }

  // A turn answering this client's tools on a conversation: their results as
  // tool messages.
  private toolResultsInput(thread: string, results: ToolResult[]): RunInput {
    const toolMessages: Message[] = results.map((result) => ({
      id: this.id(),
      role: "tool",
      toolCallId: result.toolCallId,
      content: result.content,
    }));
    const history =
      this.options.fullHistory && this.snapshot.threadId === thread
        ? this.snapshot.messages
        : [];
    return { ...this.input([...history, ...toolMessages]), threadId: thread };
  }

  // Apply protocol state before notifying application observers.
  private applyEvent(reducer: EventReducer, event: ChatEvent): void {
    if (event.type === "CUSTOM" && event.name === "hastekit.stream_id") {
      this.streamId = (event.value as { streamId?: string })?.streamId;
    }
    this.update({
      ...reducer.apply(this.snapshot, event),
      lastEvent: event,
      connection:
        event.type === "RUN_ERROR" || event.type === "RUN_FINISHED"
          ? "idle"
          : "streaming",
      isRunning: event.type !== "RUN_ERROR" && event.type !== "RUN_FINISHED",
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

// Results are text for the model; structured values travel as JSON.
function toolResultContent(value: unknown): string {
  if (typeof value === "string") return value;
  if (value === undefined) return "";
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

type ToolResult = { toolCallId: string; content: string };

// ClientToolRun follows one stream's tool calls for this client's tools.
class ClientToolRun {
  private readonly names = new Map<string, string>();
  private readonly args = new Map<string, string>();
  private readonly results = new Map<string, Promise<ToolResult>>();
  private readonly answered = new Set<string>();
  private pending?: string[];

  constructor(
    // Whether calls run as they stream in: only in a run this client started.
    private eager: boolean,
    private readonly runTool: (
      toolCallId: string,
      name: string,
      args: string,
    ) => Promise<ToolResult> | undefined,
    private readonly send: (result: ToolResult) => void,
  ) {}

  // The stream turned out to be another run's (the turn folded into it): its
  // calls may be replays of calls already answered.
  joined(): void {
    this.eager = false;
  }

  observe(event: ChatEvent): void {
    const e = event as ChatEvent & Record<string, any>;
    switch (e.type) {
      case "TOOL_CALL_START":
        this.names.set(e.toolCallId, e.toolCallName);
        break;
      case "TOOL_CALL_ARGS":
        this.args.set(
          e.toolCallId,
          (this.args.get(e.toolCallId) ?? "") + (e.delta ?? ""),
        );
        break;
      case "TOOL_CALL_END": {
        if (!this.eager) break;
        // Sent as soon as it is ready, whatever the run is doing by then: a run
        // waiting for it takes it, and a paused one resumes with it.
        const result = this.run(e.toolCallId);
        void result?.then(this.send);
        break;
      }
      case "TOOL_CALL_RESULT":
        this.answered.add(e.toolCallId);
        break;
      case "RUN_FINISHED": {
        // AG-UI 1.0: a successful run left its unanswered calls for the client.
        // The outcome may name them; otherwise they are the calls with no result.
        const outcome = e.outcome as
          { type?: string; pendingToolCallIds?: string[] } | undefined;
        if (outcome && outcome.type !== "success") break;
        this.pending = outcome?.pendingToolCallIds?.length
          ? outcome.pendingToolCallIds
          : [...this.names.keys()].filter((id) => !this.answered.has(id));
        break;
      }
    }
  }

  private run(toolCallId: string): Promise<ToolResult> | undefined {
    const name = this.names.get(toolCallId);
    if (!name) return undefined;
    let result = this.results.get(toolCallId);
    if (!result) {
      result = this.runTool(toolCallId, name, this.args.get(toolCallId) ?? "");
      if (result) this.results.set(toolCallId, result);
    }
    return result;
  }

  // The results a finished run left waiting on, when every call is this
  // client's: calls a joined run left are run now.
  pausedOn(): Promise<ToolResult>[] | undefined {
    if (!this.pending?.length) return undefined;
    const results: Promise<ToolResult>[] = [];
    for (const id of this.pending) {
      const result = this.answered.has(id) ? undefined : this.run(id);
      if (!result) return undefined;
      results.push(result);
    }
    return results;
  }
}

import { HttpAgent, randomUUID, type AgentSubscriber, type HttpAgentConfig } from "@ag-ui/client";
import type { BaseEvent, InputContent, Message, ResumeEntry, RunAgentInput, Tool } from "@ag-ui/core";
import type { Observable } from "rxjs";
import { runClientTool, withClientTools, type ClientToolRunner, type ToolResult } from "./client-tools";
import { endpoints, request } from "./http";
import { resumableEvents } from "./stream";
import { HastekitEvent, type HastekitConnection, type MCPSelection, type SkillSelection } from "./types";

export interface HastekitAgentConfig extends Omit<HttpAgentConfig, "url"> {
  /** The name the agent is registered under on the HasteKit server. Also the default `agentId`. */
  agentName: string;
  /** Base URL of the HasteKit AG-UI handler. Defaults to `/api/agui`. */
  baseUrl?: string;
  /**
   * Post the whole conversation on every run. Only for servers that do not
   * persist threads; by default the server loads the thread itself and only
   * the new turn is sent.
   */
  fullHistory?: boolean;
  skillSelection?: SkillSelection;
  mcpSelection?: MCPSelection;
}

// How long a rejoin waits for a run to claim the thread before answering 204.
const REJOIN_WAIT_SECONDS = 20;

// The run feed can report a run this client just started a moment before the
// run reports itself started here. Waiting this long before joining lets it
// declare itself, so a run is never streamed twice.
const JOIN_SETTLE_MS = 250;

// Roles a client contributes. Everything else is the server's own output echoed back.
const CLIENT_ROLES = new Set<string>(["user", "system", "developer"]);

/**
 * Candidates for this turn: the trailing block of client-authored
 * messages, or trailing tool messages when the client is returning results for
 * its own tools. Empty for a resume, whose answers travel in `resume`.
 *
 * Mirrors the server's rule (RunAgentInput.NewTurnSDKMessages). A failed run
 * may leave no assistant message separating turns, so the caller must also
 * exclude messages already submitted or loaded from the server.
 */
export function newTurnOf(messages: Message[]): Message[] {
  let start = messages.length;
  if (start > 0 && messages[start - 1].role === "tool") {
    while (start > 0 && messages[start - 1].role === "tool") start--;
    return messages.slice(start);
  }
  while (start > 0 && CLIENT_ROLES.has(messages[start - 1].role)) start--;
  return messages.slice(start);
}

// The id the server gives a client-authored message (normalizeMessageID on the
// server). A run echoes each turn back under this id, so the two must agree for
// a client to recognise its own turn.
function serverIdOf(id: string): string {
  return !id || id.startsWith("msg") ? id : `msg_${id}`;
}

/**
 * An AG-UI agent for a HasteKit server. Use it anywhere an `HttpAgent` goes,
 * such as CopilotKit's `selfManagedAgents`.
 *
 * On top of a plain `HttpAgent` it:
 *
 * - stops runs on the server: `abortRun` asks the server to cancel the run and
 *   keeps reading its stream until the run winds down, instead of only
 *   dropping the connection while the run carries on;
 * - survives dropped connections by rejoining the thread's stream where it
 *   left off, and joins runs started elsewhere (`connectAgent`, `joinIfIdle`);
 * - sends only the new turn, since the server keeps the thread;
 * - keeps history loaded from the server on screen when CopilotKit clears the
 *   agent to reconnect it (pass it as `initialMessages`);
 * - takes follow-up messages while a run is going (`steer`);
 * - answers interrupts from a page that never saw the run that raised them
 *   (`resume`);
 * - runs client tools as soon as their calls stream in (`clientToolRunner`,
 *   set by `useHastekitClientTools`);
 * - sends the user's skill and MCP server selection with every run.
 */
export class HastekitAgent extends HttpAgent {
  /** Runs tools this client provides. Set by `useHastekitClientTools`. */
  clientToolRunner?: ClientToolRunner;
  /**
   * The tools this client provides, as CopilotKit would send them now. Sent
   * with the requests this agent makes itself (steer, resume, client tool
   * results), so the run they start can still call the client's tools. Set by
   * `useHastekitClientTools`; without it those requests reuse the tools of the
   * last run or connect this agent saw.
   */
  clientTools?: () => Tool[];
  /** The user's own skills to turn off, sent with every run. */
  skillSelection: SkillSelection;
  /** The user's own MCP servers to turn off, sent with every run. */
  mcpSelection: MCPSelection;

  private name: string;
  private baseUrl?: string;
  private fullHistory: boolean;

  // Per-instance run bookkeeping; see bind().
  private history: Message[] = [];
  // Loaded or submitted messages remain part of the transcript, even when a
  // run fails before replying. They must not become input to the next turn.
  // Store server-normalized IDs so an echoed user message stays recognised.
  private sentMessages = new Set<string>();
  private steered: Message[] = [];
  private acked = new Set<string>();
  private active = false;
  private streamId?: string;
  private internal?: AgentSubscriber;
  // What CopilotKit last gave a run or connect besides its messages — the
  // client's tools, the app's context — for the requests this agent makes
  // itself (steer, resume, client tool results), which it does not hand them.
  private lastRun?: Pick<RunAgentInput, "tools" | "context" | "forwardedProps">;
  // Calls answerClientTools has already answered.
  private answeredCalls = new Set<string>();

  constructor(config: HastekitAgentConfig) {
    const { agentName, baseUrl, fullHistory, skillSelection, mcpSelection, ...rest } = config;
    super({ ...rest, agentId: rest.agentId ?? agentName, url: endpoints({ agentName, baseUrl }).run });
    this.name = agentName;
    this.baseUrl = baseUrl;
    this.fullHistory = fullHistory ?? false;
    this.skillSelection = skillSelection ?? { disable: [] };
    this.mcpSelection = mcpSelection ?? { disable: [] };
    this.bind(config.initialMessages ?? []);
  }

  /** The agent's name on the HasteKit server. */
  get agentName(): string {
    return this.name;
  }

  /** Connection details for the standalone helpers, such as `fetchThreadMessages`. */
  get connection(): HastekitConnection {
    return { agentName: this.name, baseUrl: this.baseUrl, headers: this.headers, fetch: this.fetch };
  }

  clone(): HastekitAgent {
    const copy = super.clone() as HastekitAgent;
    copy.name = this.name;
    copy.baseUrl = this.baseUrl;
    copy.fullHistory = this.fullHistory;
    copy.skillSelection = { disable: [...this.skillSelection.disable] };
    copy.mcpSelection = { disable: [...this.mcpSelection.disable] };
    copy.clientToolRunner = this.clientToolRunner;
    copy.clientTools = this.clientTools;
    copy.lastRun = this.lastRun;
    // The internal subscriber is bound to this instance; the copy gets its own.
    copy.subscribers = copy.subscribers.filter((subscriber) => subscriber !== this.internal);
    copy.bind([...this.history]);
    copy.sentMessages = new Set(this.sentMessages);
    return copy;
  }

  // ── Runs ───────────────────────────────────────────────────────────

  run(input: RunAgentInput): Observable<BaseEvent> {
    this.remember(input);
    return withClientTools(this.stream(this.url, this.requestInit(input)), {
      runTool: this.clientToolRunner,
      send: (results) => this.sendToolResults(input, results),
    });
  }

  // Attaches to whatever the thread has running: the server replays the run
  // so far and then follows it live. The client's tools answer, when the run
  // ends, calls still waiting for them — not as the replay goes by, since
  // those may already have been run.
  connect(input: RunAgentInput): Observable<BaseEvent> {
    this.remember(input);
    return withClientTools(
      this.stream(endpoints(this.connection).stream(this.threadId), { method: "GET", headers: this.headers }),
      {
        runTool: this.clientToolRunner,
        send: (results) => this.sendToolResults(this.withLastRun(input), results),
        eager: false,
      },
    );
  }

  /**
   * Answers client tool calls a paused thread is waiting on — the
   * `pendingToolCallIds` of a thread's run state, when a page opens it. The
   * calls are found in the agent's messages and run with `clientToolRunner`
   * (see `useHastekitClientTools`); the results resume the thread, and the
   * run they start is joined. A call is answered once, however often this is
   * called for it.
   */
  async answerClientTools(toolCallIds: string[]): Promise<void> {
    const runTool = this.clientToolRunner;
    if (!runTool) return;
    const wanted = new Set(toolCallIds.filter((id) => !this.answeredCalls.has(id)));
    const pending: Promise<ToolResult>[] = [];
    for (const message of this.messages) {
      if (message.role !== "assistant") continue;
      for (const call of message.toolCalls ?? []) {
        if (!wanted.has(call.id)) continue;
        const result = runClientTool(runTool, call.function.name, call.function.arguments, call.id);
        if (!result) continue;
        this.answeredCalls.add(call.id);
        pending.push(result);
      }
    }
    if (!pending.length) return;
    const run = await this.sendToolResults(this.withLastRun(this.prepareRunAgentInput()), await Promise.all(pending));
    // The run the results started is shown by joining the thread, like any run
    // this page did not start itself.
    run?.subscribe({ error: () => {} }).unsubscribe();
    await this.joinIfIdle();
  }

  /**
   * Stops the run on the server. The run winds down on its own stream, which
   * stays open until RUN_FINISHED. Falls back to dropping the connection before
   * the server has named the run's stream, or if the stop request fails.
   */
  abortRun(): void {
    const streamId = this.streamId;
    if (!streamId) {
      super.abortRun();
      return;
    }
    void request(this.connection, endpoints(this.connection).stop, {
      method: "POST",
      // A stop is worth delivering even if the page is closing.
      keepalive: true,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ threadId: this.threadId, streamId }),
    }).catch((error) => {
      console.warn("[hastekit] stop request failed; dropping the stream instead", error);
      super.abortRun();
    });
  }

  /**
   * Sends a message into the run that is already going. The server folds it
   * into that run, whose stream carries the reply.
   *
   * This does not go through a new run: starting one would detach the run in
   * flight. If that run finished just before the message landed, the server
   * starts a new run instead, which this agent then joins.
   */
  async steer(content: string | InputContent[]): Promise<void> {
    if (typeof content === "string" ? !content.trim() : !content.length) return;
    const message: Message = { id: randomUUID(), role: "user", content };
    this.steered.push(message);
    // Renders the message at once; the run in flight does not see it, which
    // is why it is also held in `steered`.
    this.addMessage(message);

    const prepared = this.withLastRun(this.prepareRunAgentInput());
    const input = { ...prepared, messages: this.fullHistory ? prepared.messages : [message] };
    const withdraw = () => {
      this.steered = this.steered.filter((m) => m.id !== message.id);
      this.setMessages(this.messages.filter((m) => m.id !== message.id));
    };
    let response: Response;
    try {
      response = await this.fetch(this.url, this.requestInit(input));
    } catch (error) {
      withdraw();
      throw error;
    }
    if (response.status === 204) return;
    void response.body?.cancel();
    if (!response.ok) {
      withdraw();
      throw new Error(`Message could not be sent (${response.status}).`);
    }
    void this.connectAgent();
  }

  /**
   * Answers interrupts the thread is waiting on, from a page that never saw the
   * run that raised them (after a reload, say). While a run's interrupt is live,
   * CopilotKit's `useInterrupt` answers it instead.
   *
   * The run this starts is joined through the thread's stream.
   */
  async resume(entries: ResumeEntry[]): Promise<void> {
    const input = { ...this.withLastRun(this.prepareRunAgentInput()), messages: [], resume: entries } as RunAgentInput;
    const response = await this.fetch(this.url, this.requestInit(input));
    void response.body?.cancel();
    if (!response.ok) throw new Error(`Resume failed (${response.status}).`);
    // 204: folded into a run that started in the meantime. Either way the
    // thread's stream is where the answer appears.
    void this.connectAgent();
  }

  /**
   * Joins the thread's current run unless this agent is already streaming one.
   * Call it when a run feed (see `watchRuns`) reports a run starting on this
   * thread elsewhere.
   */
  async joinIfIdle(): Promise<void> {
    if (this.active) return;
    await new Promise((done) => setTimeout(done, JOIN_SETTLE_MS));
    if (this.active) return;
    // Nothing runs on a thread waiting on an interrupt until it is answered, and
    // @ag-ui/client refuses to connect while one is open.
    if (this.pendingInterrupts.length > 0) return;
    await this.connectAgent();
  }

  /** Adds an older page of history ahead of the messages already loaded. */
  prependMessages(messages: Message[]): void {
    for (const message of messages) this.sentMessages.add(serverIdOf(message.id));
    const stored = new Set(this.history.map((m) => m.id));
    this.history = [...messages.filter((m) => !stored.has(m.id)), ...this.history];
    const visible = new Set(this.messages.map((m) => m.id));
    this.setMessages([...messages.filter((m) => !visible.has(m.id)), ...this.messages]);
  }

  // ── Request body ───────────────────────────────────────────────────

  // A connect carries the same tools and context as a run, which is how a
  // page that has only rejoined a thread has them.
  private remember(input: RunAgentInput): void {
    this.lastRun = { tools: input.tools, context: input.context, forwardedProps: input.forwardedProps };
  }

  // Fills in the client's tools and CopilotKit's context for a request
  // CopilotKit did not start. The server knows a client's tools only from the
  // request that starts a run: without them, that run could not call them.
  private withLastRun(input: RunAgentInput): RunAgentInput {
    const tools = input.tools.length ? input.tools : (this.clientTools?.() ?? this.lastRun?.tools ?? []);
    if (!this.lastRun) return { ...input, tools };
    return {
      ...input,
      tools,
      context: input.context.length ? input.context : this.lastRun.context,
      forwardedProps: { ...this.lastRun.forwardedProps, ...input.forwardedProps },
    };
  }

  // Trimming happens here rather than in run(): the run's input is also what
  // the event pipeline and subscribers see as the transcript, and requestInit
  // is the last place the input is only a request body.
  protected requestInit(input: RunAgentInput): RequestInit {
    const messages = this.fullHistory
      ? input.messages
      : newTurnOf(input.messages).filter((message) => !this.sentMessages.has(serverIdOf(message.id)));
    const init = super.requestInit({
      ...input,
      messages,
      forwardedProps: { ...input.forwardedProps, skills: this.skillSelection, mcp: this.mcpSelection },
    });
    // Remember the attempt, not just successful replies: the server persists
    // input before calling the model, which can then fail without a reply.
    for (const message of messages) this.sentMessages.add(serverIdOf(message.id));
    return init;
  }

  // ── Internals ──────────────────────────────────────────────────────

  private stream(url: string, init: RequestInit, first?: Response): Observable<BaseEvent> {
    return resumableEvents(this.fetch, url, init, endpoints(this.connection).stream(this.threadId, REJOIN_WAIT_SECONDS), first);
  }

  // Sends this client's tool results as tool messages on the thread, as any
  // AG-UI client answers its own tools. A live run takes them (204); otherwise
  // they start the run that resumes the paused one, streamed from here.
  private async sendToolResults(input: RunAgentInput, results: ToolResult[]): Promise<Observable<BaseEvent> | null> {
    const toolMessages: Message[] = results.map((result) => ({
      id: randomUUID(),
      role: "tool",
      toolCallId: result.toolCallId,
      content: result.content,
    }));
    const next: RunAgentInput = {
      ...input,
      runId: randomUUID(),
      messages: this.fullHistory ? [...this.messages, ...toolMessages] : toolMessages,
      // The run being answered was the one resumed, if any; this one is not.
      resume: undefined,
    };
    const init = this.requestInit(next);
    const response = await this.fetch(this.url, init);
    if (response.status === 204) {
      void response.body?.cancel();
      return null;
    }
    if (!response.ok) {
      void response.body?.cancel();
      throw new Error(`Tool results could not be sent (${response.status}).`);
    }
    return this.stream(this.url, init, response);
  }

  private bind(history: Message[]): void {
    this.history = history;
    this.sentMessages = new Set(history.map((message) => serverIdOf(message.id)));
    this.steered = [];
    this.acked = new Set();
    this.answeredCalls = new Set();
    this.active = false;
    this.streamId = undefined;
    this.internal = this.createSubscriber();
    this.subscribers = [this.internal, ...this.subscribers];
  }

  private createSubscriber(): AgentSubscriber {
    return {
      // CopilotKit clears the agent before connecting it to a thread, on the
      // assumption that the server replays the conversation; the server
      // replays only the current run. Restoring the loaded history here, before
      // the run's pipeline snapshots the messages, keeps it on screen.
      onRunInitialized: ({ messages }) => {
        this.active = true;
        const missing = this.history.filter((m) => !messages.some((seen) => seen.id === m.id));
        return missing.length ? { messages: [...missing, ...messages] } : undefined;
      },

      // The server's result for a call is the only true one. CopilotKit adds a
      // tool message echoing the answer when it resumes an approval; replace it
      // rather than show two.
      onToolCallResultEvent: ({ event, messages }) => {
        const index = messages.findIndex((m) => m.role === "tool" && m.toolCallId === event.toolCallId);
        if (index === -1) return;
        const updated = [...messages];
        updated[index] = { ...updated[index], content: event.content } as Message;
        return { messages: updated, stopPropagation: true };
      },

      onCustomEvent: ({ event, messages }) => {
        if (event.name === HastekitEvent.StreamId) {
          const streamId = (event.value as { streamId?: string } | undefined)?.streamId;
          if (streamId) this.streamId = streamId;
          return;
        }
        // A turn the run took in. A steered turn is replaced by the server's copy.
        if (event.name === HastekitEvent.InputMessage) {
          const incoming = event.value as Message | undefined;
          if (!incoming?.id) return;
          this.sentMessages.add(serverIdOf(incoming.id));
          const matches = (m: Message) => m.id === incoming.id || serverIdOf(m.id) === incoming.id;
          this.steered = this.steered.map((m) => (matches(m) ? incoming : m));
          return {
            messages: messages.some(matches)
              ? messages.map((m) => (matches(m) ? incoming : m))
              : [...messages, incoming],
          };
        }
      },

      // The run announces every turn it takes in, so a client that joined late
      // learns what was asked. For the client that asked, the announcement is
      // an echo of a message already on screen under the id it minted; adopting
      // the server's id settles both on the id the message will have after a
      // reload, and the echoed text is then suppressed.
      onTextMessageStartEvent: ({ event, messages }) => {
        const id = event.messageId;
        this.sentMessages.add(serverIdOf(id));
        if (messages.some((m) => m.id === id)) {
          this.acked.add(id);
          return;
        }
        const local = messages.find((m) => serverIdOf(m.id) === id);
        if (!local) return;
        this.acked.add(id);
        this.steered = this.steered.map((m) => (m.id === local.id ? { ...m, id } : m));
        return { messages: messages.map((m) => (m.id === local.id ? { ...m, id } : m)) };
      },
      onTextMessageContentEvent: ({ event }) => (this.acked.has(event.messageId) ? { stopPropagation: true } : undefined),
      onTextMessageEndEvent: ({ event }) => {
        this.acked.delete(event.messageId);
      },

      // A run's pipeline works on a copy of the messages taken when it starts,
      // so a steered turn or an older page added since would be dropped by the
      // next event that rewrites the list. Re-adding them is keyed by id and
      // settles after one event.
      onEvent: ({ messages }) => {
        const older = this.history.filter((m) => !messages.some((seen) => seen.id === m.id));
        const steered = this.steered.filter((m) => !messages.some((seen) => seen.id === m.id));
        return older.length || steered.length ? { messages: [...older, ...messages, ...steered] } : undefined;
      },

      // The stream id belongs to one run, and steered turns are now in the
      // agent's messages. A run that died mid-echo must not leave an id that
      // would silence the next run's turn.
      onRunFinalized: () => {
        this.active = false;
        this.streamId = undefined;
        this.steered = [];
        this.acked.clear();
      },
    };
  }
}

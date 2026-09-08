import {
  HttpAgent,
  randomUUID,
  runHttpRequest,
  transformHttpEventStream,
} from "@ag-ui/client";
import type { Message, RunAgentInput } from "@ag-ui/core";
import { stopRun, streamUrl } from "./api";

// rxjs is not a direct dependency; take the Observable type from the
// helper's own signature rather than adding one for a type import.
type EventStream = ReturnType<typeof transformHttpEventStream>;

// The CUSTOM event the server emits at the start of every run, carrying
// the broker stream id that identifies it (agui.CustomNameStreamID).
const STREAM_ID_EVENT = "hastekit.stream_id";

// The gap between a POST /run claiming the thread and the local run reporting
// itself started. The feed can see the claim first; pausing this long before
// acting on it lets the local run declare itself, so we do not stream a run we
// are already streaming.
const JOIN_SETTLE_MS = 250;

const sleep = (ms: number) => new Promise((done) => setTimeout(done, ms));

// Roles a client can contribute. Everything else (assistant, tool) is
// the server's own output being echoed back to it.
const INCOMING_ROLES = new Set(["user", "system", "developer"]);

// newTurnOf returns the messages that are new this turn: the trailing
// run of client-authored messages, which is everything after the last
// thing the agent itself produced.
//
// This mirrors RunAgentInput.NewTurnSDKMessages in pkg/agui/run_input.go,
// including its all-client-messages case (a first turn, where there is no
// assistant message to cut at and the whole list is the turn). The two
// must agree: the server applies the same rule to whatever it receives,
// so trimming to this block is what makes the trim invisible to it.
//
// Empty is a valid answer. An approval resume ends on an assistant
// message and contributes no new text — its decisions ride in
// forwardedProps, and the server builds the resolution from those.
function newTurnOf(messages: Message[]): Message[] {
  let start = messages.length;
  while (start > 0 && INCOMING_ROLES.has(messages[start - 1].role)) start--;
  return messages.slice(start);
}

// serverIdOf is the id the server will know a client-authored message by.
//
// This mirrors normalizeMessageID in pkg/agui/run_input.go, and the two must
// agree for the same reason newTurnOf must: the server applies its rule to
// whatever it receives, and the run then echoes the turn back under the id
// that rule produced. Without the mirror the echo looks like a turn nobody has
// seen, and the tab that sent it shows its own message twice.
function serverIdOf(id: string): string {
  if (!id || id.startsWith("msg")) return id;
  return "msg_" + id;
}

// StoppableHttpAgent adds the three things a long run needs from a browser
// that comes and goes: stopping it for real, picking it back up, and
// steering it while it works.
//
// Its default is to abort the fetch: the browser stops rendering, but the
// server never learns anything and the run carries on to completion,
// burning tokens with its tool call still running. Instead we POST to the
// stop endpoint and leave the connection open, so the server cancels the
// tool, writes the cancellation into history, and closes with
// RUN_FINISHED — the user watches it wind down rather than the transcript
// freezing mid-sentence.
//
// Aborting stays the fallback: before the server has told us the stream
// id, or if the stop request fails.
export class StoppableHttpAgent extends HttpAgent {
  private readonly agentName: string;
  private streamId?: string;

  // Whether the server needs the whole conversation posted to it. It
  // normally does not — see requestInit.
  private readonly fullHistory: boolean;

  // Turns steered into a run that was already in flight, held until that
  // run ends. A run's event pipeline clones agent.messages when it starts
  // and works from that copy (defaultApplyEvents), so a message appended
  // mid-run is missing from it, and the next event that rewrites the list
  // erases the steered turn from the chat — even though the agent received
  // it and history has it (it reappears on reload).
  private steered: Message[] = [];

  // The thread's stored history. CopilotChat rejoins the thread by itself
  // whenever it is given an explicit threadId, and CopilotKitCore.connectAgent
  // clears the agent first ("fresh restore": setMessages([]) + setState({})) on
  // the assumption that the server is the source of truth. The run's event
  // pipeline then snapshots that empty list, so the first event it applies
  // replaces the transcript with just the rejoined run — the conversation
  // vanishes until reloaded. Re-seeding it below puts it back.
  private readonly history: Message[];

  // Whether a run is going through this agent's own pipeline — one the user
  // started, or one the watch joined. The watch reads it to keep from
  // attaching a second connection to a run already being streamed, which
  // would feed the pipeline every event twice.
  private running = false;

  // Turns the run has announced taking in that this tab already has on
  // screen — its own, echoed back. The text still follows on the stream, and
  // appending it to a message that is already complete would double it, so
  // these ids are held until the echo has passed.
  private readonly acked = new Set<string>();

  constructor(config: {
    agentName: string;
    url: string;
    threadId?: string;
    history?: Message[];
    fullHistory?: boolean;
  }) {
    super({
      url: config.url,
      threadId: config.threadId,
      initialMessages: config.history,
    });
    this.agentName = config.agentName;
    this.history = config.history ?? [];
    this.fullHistory = config.fullHistory ?? false;

    this.subscribe({
      // Runs between the clear and the pipeline's snapshot, so history is
      // back before anything reads it — and it broadcasts onMessagesChanged,
      // so the chat re-renders even when there is no run to rejoin. Keyed by
      // id, so an ordinary turn (which still has its history) is untouched.
      onRunInitialized: ({ messages }) => {
        this.running = true;
        const missing = this.history.filter(
          (m) => !messages.some((seen) => seen.id === m.id)
        );
        if (missing.length === 0) return;
        return { messages: [...missing, ...messages] };
      },
      onCustomEvent: ({ event }: any) => {
        if (event?.name === STREAM_ID_EVENT && event?.value?.streamId) {
          this.streamId = event.value.streamId as string;
        }
      },
      // The run announces every turn it takes in, so a tab that joined late
      // learns what was asked and not only what was answered. For the tab that
      // did the asking the same event is an acknowledgement — this is where it
      // is recognised as one.
      //
      // The id is what tells them apart. A turn this tab sent is already on
      // screen under the id it minted, and the echo carries the id the server
      // made of it; adopting the server's id here settles the two on one name,
      // which is also the name the turn will have after a reload.
      onTextMessageStartEvent: ({ event, messages }: any) => {
        const id = event.messageId as string;
        if (messages.some((m: Message) => m.id === id)) {
          this.acked.add(id);
          return;
        }
        const local = messages.find((m: Message) => serverIdOf(m.id) === id);
        if (!local) return; // A turn this tab has not seen: let it through.
        this.acked.add(id);
        // Rebuilt rather than edited: the pipeline hands subscribers a frozen
        // clone and takes a mutation back only as a new array, so editing the
        // message in place changed nothing and the echo was added as a second,
        // empty bubble — its text having been suppressed as an echo.
        this.steered = this.steered.map((m) =>
          m.id === local.id ? { ...m, id } : m
        );
        return {
          messages: messages.map((m: Message) =>
            m.id === local.id ? { ...m, id } : m
          ),
        };
      },
      // The echo's text belongs to a message that already has it.
      onTextMessageContentEvent: ({ event }: any) =>
        this.acked.has(event.messageId) ? { stopPropagation: true } : undefined,
      onTextMessageEndEvent: ({ event }: any) => {
        this.acked.delete(event.messageId);
      },
      // onEvent mutations are applied to the running pipeline's own message
      // list, which is the only way to reach it from outside. Re-adding is
      // keyed by id, so this settles after one event and leaves the rest of
      // the run's message handling alone.
      onEvent: ({ messages }) => {
        const missing = this.steered.filter(
          (m) => !messages.some((seen) => seen.id === m.id)
        );
        if (missing.length === 0) return;
        return { messages: [...messages, ...missing] };
      },
      // The id belongs to one run: a later stop must not reach back to a
      // finished one. The steered turns are likewise done — the next run
      // seeds its snapshot from agent.messages, which now carries them.
      onRunFinalized: () => {
        this.running = false;
        this.streamId = undefined;
        this.steered = [];
        // A run that died mid-echo would otherwise leave an id here for ever,
        // and the next run's turn under that id would arrive silently.
        this.acked.clear();
      },
    });
  }

  // ── request body ───────────────────────────────────────────────────
  //
  // AbstractAgent.prepareRunAgentInput puts the agent's entire message
  // list on every run, so the POST body grows with the thread and a long
  // conversation re-uploads itself on each turn. The server does not want
  // it: it takes the new turn from the trailing user block and loads
  // everything before that from the thread itself, keyed by ThreadID. So
  // the rest is bytes on the wire that are parsed and then dropped.
  //
  // Trimming here rather than in run() keeps it to the wire: `input` is
  // also what the run's event pipeline snapshots and what subscribers see
  // as `input.messages`, and the transcript on screen is that list.
  // requestInit is the last place the input is only a request body.
  //
  // What goes is exactly what the server would have kept — the trailing
  // block of user/system/developer messages — so this changes the payload
  // and not the run. Sending that block back through the server's own
  // rule reselects the same block, which is what makes the two agree.
  //
  // Approvals are untouched: they travel in forwardedProps, so a resume
  // whose trailing block is empty still posts its decisions.
  protected requestInit(input: RunAgentInput): RequestInit {
    if (this.fullHistory) return super.requestInit(input);
    return super.requestInit({ ...input, messages: newTurnOf(input.messages) });
  }

  // ── steer ──────────────────────────────────────────────────────────
  //
  // Sends a follow-up into the run that is already going: the server folds
  // it into that run and answers 204, and the reply arrives on the stream
  // we are already reading.
  //
  // This deliberately does not go through copilotkit.runAgent(): that
  // detaches the active run before starting a new one, so the SSE the
  // in-flight run is still writing to would be dropped — and the fold
  // answers 204, leaving no stream to take its place. So we append the turn
  // to the transcript ourselves and POST it.
  //
  // Only the steered message goes over the wire. The server takes the new
  // turn from the trailing user block of what it is sent, and until the
  // assistant has said anything this turn that block would also swallow the
  // message that started the run.
  //
  // If the run finished in the gap between the composer deciding to steer
  // and this request landing, the server starts a fresh run and streams it
  // instead of folding. Nothing is reading that response, so we drop it and
  // pick the run up on the thread's own stream, the same way a rejoin does.
  async steer(text: string): Promise<void> {
    const trimmed = text.trim();
    if (!trimmed) return;

    // addMessage notifies subscribers, so the chat renders the turn at
    // once; the live run doesn't see it, hence `steered` above.
    const message: Message = {
      id: randomUUID(),
      role: "user",
      content: trimmed,
    };
    this.steered.push(message);
    this.addMessage(message);

    const input = { ...this.prepareRunAgentInput(), messages: [message] };
    const res = await fetch(this.url, this.requestInit(input));
    if (res.status === 204) return;

    void res.body?.cancel();
    if (!res.ok) {
      console.error("steer failed", res.status);
      return;
    }
    void this.connectAgent();
  }

  // ── resume ─────────────────────────────────────────────────────────
  //
  // Answers a pause the agent is still sitting on, from a page that has just
  // loaded and so never saw the interrupt event.
  //
  // The live path goes through CopilotKit's useInterrupt, whose resolve()
  // rides forwardedProps on the next run. There is no live pause to resolve
  // here — the run that raised it finished, which is how a pause is reported —
  // so the decisions are posted the same way that hook would have posted them.
  //
  // No messages: a resume contributes no new turn. The server builds the
  // resolution from the decisions alone.
  async resume(decisions: unknown[]): Promise<void> {
    const input = {
      ...this.prepareRunAgentInput(),
      messages: [],
      forwardedProps: { command: { resume: { decisions } } },
    };

    const res = await fetch(this.url, this.requestInit(input as RunAgentInput));
    if (res.status === 204) {
      // Folded into a run that had started in the meantime; its stream is
      // where the answer appears.
      void this.connectAgent();
      return;
    }
    if (!res.ok) {
      void res.body?.cancel();
      throw new Error(`resume → ${res.status}`);
    }

    // Nothing is reading this response, so drop it and pick the run up on the
    // thread's own stream — the same way a rejoin does.
    void res.body?.cancel();
    void this.connectAgent();
  }

  // connect attaches to whatever this thread already has running, instead
  // of starting a turn. AbstractAgent.connect throws by default — nothing
  // to attach to over plain HTTP — so this points it at the thread's
  // stream endpoint, which replays the run so far and then follows it
  // live. connectAgent() feeds the result through the same pipeline a
  // normal run uses, so messages land in the chat as they would have.
  protected connect(_input: RunAgentInput): EventStream {
    return transformHttpEventStream(
      runHttpRequest(streamUrl(this.agentName, this.threadId), {
        method: "GET",
        headers: { Accept: "text/event-stream" },
      })
    );
  }

  // ── joining a run nobody here started ──────────────────────────────
  //
  // The run feed says something began on this thread; this attaches to it.
  //
  // Kept apart from the feed on purpose. The feed reports that a run exists;
  // joining goes through the ordinary rejoin, so a run this browser started
  // is never streamed twice into the same pipeline.
  async joinIfIdle(): Promise<void> {
    // A run of our own owns the thread until it finishes. Attaching a second
    // connection to it would feed the pipeline every event twice.
    if (this.running) return;

    // Let a run this browser just started declare itself before we decide it
    // is somebody else's: the POST claims the thread a moment before the
    // local run reports itself started, and the feed can see the claim first.
    await sleep(JOIN_SETTLE_MS);
    if (this.running) return;

    // connectAgent feeds the thread's stream through the same pipeline a
    // normal run uses, so the messages land in the chat as they would have,
    // and resolves when the run ends.
    await this.connectAgent();
  }

  abortRun(): void {
    const streamId = this.streamId;
    if (!streamId) {
      super.abortRun();
      return;
    }
    // Not awaited: the button responds now; the outcome shows up on the
    // run's own stream.
    void stopRun(this.agentName, streamId).catch((e) => {
      console.error("stop request failed; aborting the stream instead", e);
      super.abortRun();
    });
  }
}

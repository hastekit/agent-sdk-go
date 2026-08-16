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
        this.streamId = undefined;
        this.steered = [];
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

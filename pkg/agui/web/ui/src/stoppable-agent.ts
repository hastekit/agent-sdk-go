import {
  HttpAgent,
  runHttpRequest,
  transformHttpEventStream,
} from "@ag-ui/client";
import type { RunAgentInput } from "@ag-ui/core";
import { stopRun, streamUrl } from "./api";

// rxjs is not a direct dependency; take the Observable type from the
// helper's own signature rather than adding one for a type import.
type EventStream = ReturnType<typeof transformHttpEventStream>;

// The CUSTOM event the server emits at the start of every run, carrying
// the broker stream id that identifies it (agui.CustomNameStreamID).
const STREAM_ID_EVENT = "hastekit.stream_id";

// StoppableHttpAgent adds the two things a long run needs from a browser
// that comes and goes: stopping it for real, and picking it back up.
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

  constructor(config: { agentName: string; url: string; threadId?: string }) {
    super({ url: config.url, threadId: config.threadId });
    this.agentName = config.agentName;

    this.subscribe({
      onCustomEvent: ({ event }: any) => {
        if (event?.name === STREAM_ID_EVENT && event?.value?.streamId) {
          this.streamId = event.value.streamId as string;
        }
      },
      // The id belongs to one run: a later stop must not reach back to a
      // finished one.
      onRunFinalized: () => {
        this.streamId = undefined;
      },
    });
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

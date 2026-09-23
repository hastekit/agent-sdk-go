import type { ChatEvent, StreamOptions } from "./types.js";
import { InvalidStreamError, SSEDecoder } from "./sse.js";

// Re-export protocol errors alongside the stream transport errors.
export { InvalidStreamError } from "./sse.js";

// Preserve HTTP status codes for authentication and retry decisions.
export class HTTPError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "HTTPError";
  }
}
// An expired replay stream requires history recovery rather than another GET.
export class StreamUnavailableError extends Error {}

// Cancel retry delays immediately when the consumer leaves the conversation.
export function delay(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const done = () => {
      clearTimeout(timer);
      signal.removeEventListener("abort", done);
      resolve();
    };
    const timer = setTimeout(done, ms);
    signal.addEventListener("abort", done, { once: true });
    if (signal.aborted) done();
  });
}

// Keep stream configuration separate from REST endpoint construction.
export interface ResumableOptions extends StreamOptions {
  fetch: typeof fetch;
  headers: () => Promise<Headers>;
  credentials?: RequestCredentials;
  initialURL: string;
  rejoinURL: string;
  body?: string;
  retryDelayMs?: number;
  maxRetries?: number;
  idleTimeoutMs?: number;
}
/** One POST at most. All recovery uses GET and acknowledges complete frames only. */
export async function* resumableStream(
  options: ResumableOptions,
): AsyncGenerator<ChatEvent> {
  let reconnect = false,
    cursor = "",
    failures = 0;
  // Only retained for this connection lifetime, never across history reloads.
  const seen = new Set<string>();

  // Keep one logical event pipeline alive across recoverable disconnects.
  while (!options.signal.aborted) {
    // Isolate the idle watchdog from cancellation of the overall subscription.
    const attempt = new AbortController();
    const abort = () => attempt.abort();
    options.signal.addEventListener("abort", abort, { once: true });

    // Reset inactivity on every network chunk, including heartbeat comments.
    let timer: ReturnType<typeof setTimeout> | undefined;
    const touch = () => {
      clearTimeout(timer);
      timer = setTimeout(abort, options.idleTimeoutMs ?? 45_000);
    };

    // Retain the reader so every exit path can release the response body.
    let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
    try {
      options.onStatus?.(reconnect ? "reconnecting" : "connecting");
      touch();

      // Resolve fresh credentials for every attempt and preserve the last applied cursor.
      const headers = await options.headers();
      if (options.signal.aborted) return;
      headers.set("Accept", "text/event-stream");
      if (reconnect && cursor) headers.set("Last-Event-ID", cursor);
      const retrying = reconnect;

      // Mark BEFORE fetch: a failed POST can still have reached the server.
      reconnect = true;
      const response = await options.fetch(
        retrying ? options.rejoinURL : options.initialURL,
        {
          method: !retrying && options.body !== undefined ? "POST" : "GET",
          body: retrying ? undefined : options.body,
          headers,
          credentials: options.credentials,
          signal: attempt.signal,
        },
      );

      // An initial empty GET is idle; an empty replay means its history must be restored.
      if (response.status === 204) {
        if (retrying)
          throw new StreamUnavailableError(
            "The run stream is no longer available. Reloading history is required.",
          );
        if (options.body !== undefined) continue; // Turn folded into an existing run.
        return;
      }

      // Preserve non-success status codes so permanent failures bypass retries.
      if (!response.ok)
        throw new HTTPError(
          response.status,
          (await response.text()) || `HTTP ${response.status}`,
        );

      // A successful response must provide a readable event body.
      reader = response.body?.getReader();
      if (!reader) throw new Error("Stream response has no body");
      options.onStatus?.("streaming");
      // Each HTTP response owns its decoder, so truncated frames are never reused.
      const decoder = new SSEDecoder();
      while (!options.signal.aborted) {
        const chunk = await reader.read();
        if (chunk.done) break;
        touch();

        // A replay may repeat an acknowledged frame; apply each event identity once.
        for (const { id, event } of decoder.push(chunk.value)) {
          if (id && seen.has(id)) continue;

          // Yield before acknowledging, so processing failures cannot advance the cursor.
          yield event;
          if (id !== undefined) {
            cursor = id;
            if (id) seen.add(id);
          }
          failures = 0;
          if (event.type === "RUN_FINISHED" || event.type === "RUN_ERROR")
            return;
        }
      }
    } catch (error) {
      // Cancellation is expected; malformed data and permanent HTTP failures are not retryable.
      if (options.signal.aborted) return;
      if (
        error instanceof InvalidStreamError ||
        error instanceof StreamUnavailableError ||
        (error instanceof HTTPError &&
          error.status < 500 &&
          error.status !== 408 &&
          error.status !== 429)
      )
        throw error;
      if (failures >= (options.maxRetries ?? 8)) throw error;
    } finally {
      // Release timers, readers, and listeners before opening another connection.
      clearTimeout(timer);
      void reader?.cancel().catch(() => {});
      attempt.abort();
      options.signal.removeEventListener("abort", abort);
    }

    // Bound reconnect attempts and delay them without preventing immediate cancellation.
    if (options.signal.aborted) return;
    if (failures >= (options.maxRetries ?? 8))
      throw new Error("Stream reconnect limit exceeded");
    options.onStatus?.("reconnecting");
    await delay(
      Math.min((options.retryDelayMs ?? 500) * 2 ** failures++, 10_000),
      options.signal,
    );
  }
}

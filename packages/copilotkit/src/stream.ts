import { EventSchemas, type BaseEvent } from "@ag-ui/core";
import { Observable } from "rxjs";

// A stream that has gone quiet this long is treated as dropped and rejoined.
const IDLE_TIMEOUT_MS = 45_000;

class FatalStreamError extends Error {}

type Fetch = (url: string, init: RequestInit) => Promise<Response>;

/**
 * Streams a run's events, rejoining the thread's stream when the connection
 * drops so that one pipeline sees the whole run.
 *
 * The first request is sent as given, unless its response is passed as
 * `first` because the caller had to see its status. Every retry is a GET of `rejoinURL` with
 * the last event id received, so the server replays only what was missed and a
 * POST that may already have started a run is never repeated. A 204 on the
 * first request means there is nothing to stream; on a retry it means the run
 * is no longer retained, which is fatal.
 */
export function resumableEvents(
  fetch: Fetch,
  initialURL: string,
  initialInit: RequestInit,
  rejoinURL: string,
  first?: Response,
): Observable<BaseEvent> {
  return new Observable<BaseEvent>((subscriber) => {
    const lifetime = new AbortController();
    const abort = () => {
      lifetime.abort();
      subscriber.complete();
    };
    initialInit.signal?.addEventListener("abort", abort, { once: true });
    if (initialInit.signal?.aborted) abort();

    let lastEventId = "";
    let reconnect = false;
    let failures = 0;

    const pause = (ms: number) =>
      new Promise<void>((resolve) => {
        const finish = () => {
          clearTimeout(timer);
          lifetime.signal.removeEventListener("abort", finish);
          resolve();
        };
        const timer = setTimeout(finish, ms);
        lifetime.signal.addEventListener("abort", finish, { once: true });
        if (lifetime.signal.aborted) finish();
      });

    void (async () => {
      while (!lifetime.signal.aborted && !subscriber.closed) {
        const attempt = new AbortController();
        const abortAttempt = () => attempt.abort();
        lifetime.signal.addEventListener("abort", abortAttempt, { once: true });
        let idle: ReturnType<typeof setTimeout> | undefined;
        const resetIdle = () => {
          clearTimeout(idle);
          idle = setTimeout(abortAttempt, IDLE_TIMEOUT_MS);
        };
        let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
        try {
          resetIdle();
          const headers = new Headers(initialInit.headers);
          headers.set("Accept", "text/event-stream");
          if (reconnect && lastEventId) headers.set("Last-Event-ID", lastEventId);
          const retrying = reconnect;
          const response =
            !reconnect && first
              ? first
              : await fetch(
                  reconnect ? rejoinURL : initialURL,
                  reconnect
                    ? { method: "GET", headers, credentials: initialInit.credentials, signal: attempt.signal }
                    : { ...initialInit, headers, signal: attempt.signal },
                );
          reconnect = true; // Never repeat a POST that may have started a run.
          if (response.status === 204) {
            if (retrying) throw new FatalStreamError("The interrupted run is no longer available; reload the thread.");
            subscriber.complete();
            return;
          }
          if (!response.ok) {
            const text = await response.text();
            if (response.status >= 500 || response.status === 408 || response.status === 429) throw new Error(text);
            throw new FatalStreamError(`Stream ${response.status}: ${text}`);
          }
          reader = response.body?.getReader();
          if (!reader) throw new Error("Stream response has no body");
          // A body fetched before this attempt does not follow its signal.
          const current = reader;
          attempt.signal.addEventListener("abort", () => void current.cancel().catch(() => {}), { once: true });

          const decoder = new TextDecoder();
          let pending = "";
          while (!lifetime.signal.aborted) {
            const { done, value } = await reader.read();
            if (done) break;
            resetIdle();
            pending = (pending + decoder.decode(value, { stream: true })).replace(/\r\n/g, "\n");
            let boundary: number;
            while ((boundary = pending.indexOf("\n\n")) !== -1) {
              const frame = pending.slice(0, boundary);
              pending = pending.slice(boundary + 2);
              let id: string | undefined;
              const data: string[] = [];
              for (const line of frame.split("\n")) {
                if (line.startsWith("id:")) id = line.slice(3).replace(/^ /, "");
                if (line.startsWith("data:")) data.push(line.slice(5).replace(/^ /, ""));
              }
              if (!data.length) continue;
              let event: BaseEvent;
              try {
                event = EventSchemas.parse(JSON.parse(data.join("\n"))) as BaseEvent;
              } catch (error) {
                throw new FatalStreamError(`Invalid stream event: ${error}`);
              }
              subscriber.next(event);
              if (id !== undefined && !id.includes("\0")) lastEventId = id;
              failures = 0;
              if (event.type === "RUN_FINISHED" || event.type === "RUN_ERROR") {
                subscriber.complete();
                return;
              }
            }
          }
          // EOF without a terminal event is a disconnect. The partial frame is
          // dropped; only the last complete, validated event is acknowledged.
        } catch (error) {
          reconnect = true;
          if (lifetime.signal.aborted || subscriber.closed) return;
          if (error instanceof FatalStreamError) {
            subscriber.error(error);
            return;
          }
        } finally {
          clearTimeout(idle);
          void reader?.cancel().catch(() => {});
          attempt.abort();
          lifetime.signal.removeEventListener("abort", abortAttempt);
        }
        if (lifetime.signal.aborted || subscriber.closed) return;
        await pause(Math.min(500 * 2 ** Math.min(failures++, 5), 10_000));
      }
    })().catch((error) => {
      if (!subscriber.closed) subscriber.error(error);
    });

    return () => {
      lifetime.abort();
      initialInit.signal?.removeEventListener("abort", abort);
    };
  });
}

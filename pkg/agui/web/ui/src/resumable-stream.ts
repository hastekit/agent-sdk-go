import { Observable } from "rxjs";
import { EventSchemas, type BaseEvent } from "@ag-ui/core";

class FatalStreamError extends Error {}

// Reconnect inside one event pipeline so open text/tool state is preserved.
// Cursors live only for this subscription, never across full page reloads.
export function resumableEvents(initialURL: string, initialInit: RequestInit, rejoinURL: string): Observable<BaseEvent> {
  return new Observable<BaseEvent>((subscriber) => {
    const lifetime = new AbortController();
    const abort = () => { lifetime.abort(); subscriber.complete(); };
    initialInit.signal?.addEventListener("abort", abort, { once: true });
    if (initialInit.signal?.aborted) abort();
    let lastEventID = "", reconnect = false, failures = 0;
    const pause = (ms: number) => new Promise<void>((resolve) => {
      const finish = () => { clearTimeout(timer); lifetime.signal.removeEventListener("abort", finish); resolve(); };
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
        const resetIdle = () => { clearTimeout(idle); idle = setTimeout(abortAttempt, 45_000); };
        let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
        try {
          resetIdle();
          const headers = new Headers(initialInit.headers);
          headers.set("Accept", "text/event-stream");
          if (reconnect && lastEventID) headers.set("Last-Event-ID", lastEventID);
          const retrying = reconnect;
          const response = await fetch(reconnect ? rejoinURL : initialURL, reconnect
            ? { method: "GET", headers, credentials: initialInit.credentials, signal: attempt.signal }
            : { ...initialInit, headers, signal: attempt.signal });
          reconnect = true; // Never repeat a POST that may have started a run.
          if (response.status === 204) {
            if (retrying) throw new FatalStreamError("The interrupted run is no longer available; reload thread history.");
            subscriber.complete(); return;
          }
          if (!response.ok) {
            const text = await response.text();
            if (response.status >= 500 || response.status === 408 || response.status === 429) throw new Error(text);
            throw new FatalStreamError(`Stream ${response.status}: ${text}`);
          }
          reader = response.body?.getReader();
          if (!reader) throw new Error("Stream response has no body");
          const decoder = new TextDecoder();
          let pending = "";
          while (!lifetime.signal.aborted) {
            const { done, value } = await reader.read();
            if (done) break;
            resetIdle();
            pending += decoder.decode(value, { stream: true });
            pending = pending.replace(/\r\n/g, "\n");
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
              try { event = EventSchemas.parse(JSON.parse(data.join("\n"))) as BaseEvent; }
              catch (error) { throw new FatalStreamError(`Invalid stream event: ${error}`); }
              subscriber.next(event);
              if (id !== undefined && !id.includes("\0")) lastEventID = id;
              failures = 0;
              if (event.type === "RUN_FINISHED" || event.type === "RUN_ERROR") { subscriber.complete(); return; }
            }
          }
          // EOF without a terminal event is a disconnect. Discard the partial
          // frame and acknowledge only the last complete, validated event.
        } catch (error) {
          reconnect = true;
          if (lifetime.signal.aborted || subscriber.closed) return;
          if (error instanceof FatalStreamError) { subscriber.error(error); return; }
        } finally {
          clearTimeout(idle);
          void reader?.cancel().catch(() => {});
          attempt.abort();
          lifetime.signal.removeEventListener("abort", abortAttempt);
        }
        if (lifetime.signal.aborted || subscriber.closed) return;
        await pause(Math.min(500 * 2 ** Math.min(failures++, 5), 10_000));
      }
    })().catch((error) => { if (!subscriber.closed) subscriber.error(error); });
    return () => {
      lifetime.abort();
      initialInit.signal?.removeEventListener("abort", abort);
    };
  });
}

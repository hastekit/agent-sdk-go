import type { BaseEvent } from "@ag-ui/core";
import type { Observable } from "rxjs";
import { describe, expect, it } from "vitest";
import { resumableEvents } from "../src/stream";

const frame = (id: string, event: object) => `id: ${id}\ndata: ${JSON.stringify(event)}\n\n`;
const start = { type: "RUN_STARTED", threadId: "thread", runId: "client" };
const finish = { type: "RUN_FINISHED", threadId: "thread", runId: "client" };

const sse = (...parts: string[]) =>
  new Response(
    new ReadableStream({
      start(controller) {
        for (const part of parts) controller.enqueue(new TextEncoder().encode(part));
        controller.close();
      },
    }),
    { headers: { "Content-Type": "text/event-stream" } },
  );

const collect = (stream: Observable<BaseEvent>) =>
  new Promise<any[]>((resolve, reject) => {
    const events: any[] = [];
    stream.subscribe({ next: (event) => events.push(event), error: reject, complete: () => resolve(events) });
  });

describe("resumableEvents", () => {
  it("rejoins after a dropped connection, acknowledging only complete frames and never repeating the POST", async () => {
    const requests: { url: string; init: RequestInit }[] = [];
    const partial = frame("4", { type: "TEXT_MESSAGE_CONTENT", messageId: "m", delta: "🌍" });
    const fetch = async (url: string, init: RequestInit) => {
      requests.push({ url, init });
      if (requests.length === 1) {
        return sse(
          frame("1", start) + frame("2", { type: "TEXT_MESSAGE_START", messageId: "m", role: "assistant" }),
          frame("3", { type: "TEXT_MESSAGE_CONTENT", messageId: "m", delta: "Hello " }) + partial.slice(0, 17),
        );
      }
      return sse(partial, frame("5", { type: "TEXT_MESSAGE_END", messageId: "m" }), frame("6", finish));
    };

    const events = await collect(resumableEvents(fetch, "/run", { method: "POST", body: "{}" }, "/stream"));

    expect(requests.map((r) => [r.url, r.init.method])).toEqual([
      ["/run", "POST"],
      ["/stream", "GET"],
    ]);
    expect(new Headers(requests[1].init.headers).get("Last-Event-ID")).toBe("3");
    expect(events.filter((e) => e.type === "RUN_STARTED")).toHaveLength(1);
    expect(events.filter((e) => e.type === "TEXT_MESSAGE_CONTENT").map((e) => e.delta).join("")).toBe("Hello 🌍");
    expect(events.at(-1).type).toBe("RUN_FINISHED");
  });

  it("rejoins without a cursor when the first request fails before any event", async () => {
    const calls: RequestInit[] = [];
    const fetch = async (_url: string, init: RequestInit) => {
      calls.push(init);
      if (calls.length === 1) throw new TypeError("offline");
      return sse(frame("1", start), frame("2", finish));
    };
    await collect(resumableEvents(fetch, "/run", { method: "POST" }, "/stream"));
    expect(calls.map((c) => c.method)).toEqual(["POST", "GET"]);
    expect(new Headers(calls[1].headers).has("Last-Event-ID")).toBe(false);
  });

  it("fails when the run can no longer be replayed", async () => {
    let calls = 0;
    const fetch = async () => (++calls === 1 ? sse(frame("1", start)) : new Response("Reload", { status: 410 }));
    await expect(collect(resumableEvents(fetch, "/run", { method: "POST" }, "/stream"))).rejects.toThrow(/410/);
    expect(calls).toBe(2);
  });

  it("reports a run lost while reconnecting instead of completing", async () => {
    let calls = 0;
    const fetch = async () => {
      if (++calls === 1) throw new TypeError("offline");
      return new Response(null, { status: 204 });
    };
    await expect(collect(resumableEvents(fetch, "/run", { method: "POST" }, "/stream"))).rejects.toThrow(
      /no longer available/,
    );
  });

  it("streams a response the caller already fetched, and rejoins after it", async () => {
    const urls: string[] = [];
    const fetch = async (url: string) => {
      urls.push(url);
      return sse(frame("2", finish));
    };
    const first = sse(frame("1", start));
    const events = await collect(resumableEvents(fetch, "/run", { method: "POST" }, "/stream", first));
    expect(urls).toEqual(["/stream"]);
    expect(events.map((e) => e.type)).toEqual(["RUN_STARTED", "RUN_FINISHED"]);
  });

  it("completes with nothing when there is no run to join", async () => {
    const fetch = async () => new Response(null, { status: 204 });
    await expect(collect(resumableEvents(fetch, "/stream", { method: "GET" }, "/stream"))).resolves.toEqual([]);
  });

  it("stops without reconnecting when the caller aborts", async () => {
    let calls = 0;
    const fetch = (_url: string, init: RequestInit) => {
      calls++;
      return new Promise<Response>((_resolve, reject) =>
        init.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError"))),
      );
    };
    const abort = new AbortController();
    const done = collect(resumableEvents(fetch, "/run", { method: "POST", signal: abort.signal }, "/stream"));
    abort.abort();
    await expect(done).resolves.toEqual([]);
    expect(calls).toBe(1);
  });
});

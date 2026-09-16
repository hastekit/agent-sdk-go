import test from "node:test";
import assert from "node:assert/strict";
import { readFile, writeFile, mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import ts from "typescript";

const dir = await mkdtemp(join(tmpdir(), "hastekit-stream-"));
let source = await readFile(new URL("../src/resumable-stream.ts", import.meta.url), "utf8");
let code = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ES2022, target: ts.ScriptTarget.ES2022 } }).outputText;
for (const dependency of ["rxjs", "@ag-ui/core"]) code = code.replaceAll(`"${dependency}"`, JSON.stringify(import.meta.resolve(dependency)));
const path = join(dir, "stream.mjs");
await writeFile(path, code);
const { resumableEvents } = await import(pathToFileURL(path).href);
await rm(dir, { recursive: true, force: true });

const frame = (id, event) => `id: ${id}\ndata: ${JSON.stringify(event)}\n\n`;
const start = { type: "RUN_STARTED", threadId: "thread", runId: "client" };
const finish = { type: "RUN_FINISHED", threadId: "thread", runId: "client" };
const response = (...parts) => new Response(new ReadableStream({ start(controller) {
  for (const part of parts) controller.enqueue(new TextEncoder().encode(part));
  controller.close();
} }), { headers: { "Content-Type": "text/event-stream" } });
const collect = (stream) => new Promise((resolve, reject) => {
  const events = [];
  stream.subscribe({ next: (event) => events.push(event), error: reject, complete: () => resolve(events) });
});

test("reconnect acknowledges only complete frames and never repeats the POST", async (t) => {
  const requests = [];
  const partial = frame("4", { type: "TEXT_MESSAGE_CONTENT", messageId: "m", delta: "🌍" });
  t.mock.method(globalThis, "fetch", async (url, init) => {
    requests.push({ url, init });
    if (requests.length === 1) return response(
      frame("1", start) + frame("2", { type: "TEXT_MESSAGE_START", messageId: "m", role: "assistant" }),
      frame("3", { type: "TEXT_MESSAGE_CONTENT", messageId: "m", delta: "Hello " }) + partial.slice(0, 17),
    );
    assert.equal(init.method, "GET");
    assert.equal(init.headers.get("Last-Event-ID"), "3");
    return response(partial, frame("5", { type: "TEXT_MESSAGE_END", messageId: "m" }), frame("6", finish));
  });
  const events = await collect(resumableEvents("/run", { method: "POST", body: "{}" }, "/stream"));
  assert.equal(requests.length, 2);
  assert.equal(events.filter((e) => e.type === "RUN_STARTED").length, 1);
  assert.equal(events.filter((e) => e.type === "TEXT_MESSAGE_CONTENT").map((e) => e.delta).join(""), "Hello 🌍");
  assert.equal(events.at(-1).runId, "client");
});

test("network failure before response headers rejoins without repeating a POST", async (t) => {
  const methods = [];
  t.mock.method(globalThis, "fetch", async (_url, init) => {
    methods.push(init.method);
    if (methods.length === 1) throw new TypeError("offline");
    assert.equal(init.headers.has("Last-Event-ID"), false);
    return response(frame("1", start), frame("2", finish));
  });
  await collect(resumableEvents("/run", { method: "POST" }, "/stream"));
  assert.deepEqual(methods, ["POST", "GET"]);
});

test("expired replay fails explicitly without restarting the run", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async () => {
    if (++calls === 1) return response(frame("1", start));
    return new Response("Reload thread history", { status: 410 });
  });
  await assert.rejects(collect(resumableEvents("/run", { method: "POST" }, "/stream")), /410/);
  assert.equal(calls, 2);
});

test("fresh subscriptions do not reuse the previous cursor", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (_url, init) => {
    calls++;
    assert.equal(init.headers.has("Last-Event-ID"), false);
    return response(frame("1", start), frame("2", finish));
  });
  await collect(resumableEvents("/stream", { method: "GET" }, "/stream"));
  await collect(resumableEvents("/stream", { method: "GET" }, "/stream"));
  assert.equal(calls, 2);
});

test("caller cancellation ends the stream without reconnecting", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (_url, init) => {
    calls++;
    return new Promise((_resolve, reject) => init.signal.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError"))));
  });
  const abort = new AbortController();
  const done = collect(resumableEvents("/run", { method: "POST", signal: abort.signal }, "/stream"));
  abort.abort();
  assert.deepEqual(await done, []);
  assert.equal(calls, 1);
});


test("an unavailable interrupted run is surfaced, not reported as success", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async () => {
    if (++calls === 1) throw new TypeError("offline");
    return new Response(null, { status: 204 });
  });
  await assert.rejects(collect(resumableEvents("/run", { method: "POST" }, "/stream")), /no longer available/);
  assert.equal(calls, 2);
});

import test from "node:test";
import assert from "node:assert/strict";
import { createAGUITransport } from "../dist/index.js";
import { frame, response } from "./helpers.mjs";

// Reconnect must preserve the last complete cursor without replaying the original POST.
test("reconnect uses GET, rotating credentials, and complete-frame cursors", async () => {
  const requests = [];
  let token = 0;
  const first = {
    type: "TEXT_MESSAGE_START",
    messageId: "m",
    role: "assistant",
  };
  const delta = {
    type: "TEXT_MESSAGE_CONTENT",
    messageId: "m",
    delta: "hello",
  };
  const client = createAGUITransport({
    retryDelayMs: 0,
    headers: () => ({ Authorization: `Bearer ${++token}` }),
    fetch: async (url, init) => {
      requests.push({ url, init });
      if (requests.length === 1)
        return response(frame("1", first), 'id: 2\ndata: {"type":');
      assert.equal(init.method, "GET");
      assert.equal(init.headers.get("Last-Event-ID"), "1");
      assert.equal(init.headers.get("Authorization"), "Bearer 2");
      return response(
        frame("1", first),
        frame("2", delta),
        frame("3", { type: "RUN_FINISHED" }),
      );
    },
  });
  const events = [];
  for await (const event of client.stream(
    "agent",
    "thread",
    {},
    { signal: new AbortController().signal },
  ))
    events.push(event);
  assert.equal(requests[0].init.method, "POST");
  assert.deepEqual(
    events.map((event) => event.type),
    ["TEXT_MESSAGE_START", "TEXT_MESSAGE_CONTENT", "RUN_FINISHED"],
  );
});

// A transport failure is ambiguous, so recovery must never resend a user turn.
test("network failure before response still reconnects with GET", async () => {
  const methods = [];
  const client = createAGUITransport({
    retryDelayMs: 0,
    fetch: async (_url, init) => {
      methods.push(init.method);
      if (methods.length === 1) throw new TypeError("connection reset");
      return response(frame("1", { type: "RUN_FINISHED" }));
    },
  });
  for await (const _event of client.stream(
    "a",
    "t",
    {},
    { signal: new AbortController().signal },
  )) {
  }
  assert.deepEqual(methods, ["POST", "GET"]);
});

// A folded turn joins the existing stream instead of completing the UI prematurely.
test("204 POST joins the existing run", async () => {
  const methods = [];
  const client = createAGUITransport({
    fetch: async (_url, init) => {
      methods.push(init.method);
      return methods.length === 1
        ? new Response(null, { status: 204 })
        : response(frame("1", { type: "RUN_FINISHED" }));
    },
  });
  for await (const _event of client.stream(
    "a",
    "t",
    {},
    { signal: new AbortController().signal },
  )) {
  }
  assert.deepEqual(methods, ["POST", "GET"]);
});

// Fatal authorization errors must surface immediately rather than enter a retry loop.
test("401 is not retried", async () => {
  let calls = 0;
  const client = createAGUITransport({
    fetch: async () => {
      calls++;
      return new Response("unauthorized", { status: 401 });
    },
  });
  await assert.rejects(async () => {
    for await (const _event of client.stream("a", "t", undefined, {
      signal: new AbortController().signal,
    })) {
    }
  }, /unauthorized/);
  assert.equal(calls, 1);
});

// Split CRLF and UTF-8 bytes must not corrupt rendered text or frame boundaries.
test("SSE parsing preserves split UTF-8 and CRLF", async () => {
  const bytes = new TextEncoder().encode(
    (
      frame("1", {
        type: "TEXT_MESSAGE_CONTENT",
        messageId: "m",
        delta: "🌍",
      }) + frame("2", { type: "RUN_FINISHED" })
    ).replaceAll("\n", "\r\n"),
  );
  const client = createAGUITransport({
    fetch: async () =>
      new Response(
        new ReadableStream({
          start(controller) {
            for (const byte of bytes)
              controller.enqueue(new Uint8Array([byte]));
            controller.close();
          },
        }),
      ),
  });
  const events = [];
  for await (const event of client.stream("a", "t", undefined, {
    signal: new AbortController().signal,
  }))
    events.push(event);
  assert.equal(events[0].delta, "🌍");
  assert.equal(events.length, 2);
});

// Cancellation must release a response reader even while the stream is idle.
test("abort releases an idle stream reader", async () => {
  const abort = new AbortController();
  let attemptSignal;
  const client = createAGUITransport({
    fetch: async (_url, init) => {
      attemptSignal = init.signal;
      return new Response(
        new ReadableStream({
          start(controller) {
            init.signal.addEventListener(
              "abort",
              () => controller.error(new Error("aborted")),
              { once: true },
            );
            controller.enqueue(
              new TextEncoder().encode(frame("1", { type: "RUN_STARTED" })),
            );
          },
        }),
      );
    },
  });

  // Aborting after the first event must end the fetch attempt without another request.
  for await (const _event of client.stream("a", "t", undefined, {
    signal: abort.signal,
  }))
    abort.abort();
  assert.equal(attemptSignal.aborted, true);
});

// A terminal event closes processing before any extra vendor data can be decoded.
test("terminal event wins over trailing non-AGUI data", async () => {
  const client = createAGUITransport({
    fetch: async () =>
      response(frame("1", { type: "RUN_FINISHED" }) + "data: [DONE]\n\n"),
  });
  const events = [];
  for await (const event of client.stream("a", "t", undefined, {
    signal: new AbortController().signal,
  }))
    events.push(event);
  assert.deepEqual(
    events.map((event) => event.type),
    ["RUN_FINISHED"],
  );
});

// Endless disconnects must eventually surface a bounded reconnect failure.
test("EOF retries stop at the configured limit", async () => {
  let requests = 0;
  const client = createAGUITransport({
    retryDelayMs: 0,
    maxRetries: 2,
    fetch: async () => {
      requests++;
      return response();
    },
  });
  await assert.rejects(async () => {
    for await (const _event of client.stream("a", "t", undefined, {
      signal: new AbortController().signal,
    })) {
    }
  }, /limit/);
  assert.equal(requests, 3);
});

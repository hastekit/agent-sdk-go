import assert from "node:assert/strict";

// Encode deterministic broker frames using the same SSE shape as the Go server.
export function frame(id, event) {
  return `id: ${id}\ndata: ${JSON.stringify(event)}\n\n`;
}

// Deliver arbitrary byte boundaries to exercise incremental stream decoding.
export function response(...parts) {
  return new Response(
    new ReadableStream({
      start(controller) {
        for (const part of parts)
          controller.enqueue(new TextEncoder().encode(part));
        controller.close();
      },
    }),
    { headers: { "Content-Type": "text/event-stream" } },
  );
}

// Wait for a state transition without adding long sleeps to every test.
export async function until(predicate) {
  for (let attempt = 0; attempt < 100; attempt++) {
    if (predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 2));
  }
  assert.ok(predicate(), "expected state transition did not occur");
}

// Keep one asynchronous operation under explicit test control.
export function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => {
    resolve = yes;
    reject = no;
  });
  return { promise, resolve, reject };
}

// Supply the smallest valid history page for controller tests.
export function page(messages = [], nextCursor = "") {
  return { messages, nextCursor, sessionId: "session", run: null };
}

// Provide a deterministic transport whose individual methods can be overridden.
export function transport(overrides = {}) {
  return {
    listThreads: async () => ({ supported: true, threads: [] }),
    loadMessages: async () => page(),
    stream: async function* () {},
    stop: async () => {},
    ...overrides,
  };
}

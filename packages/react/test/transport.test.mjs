import test from "node:test";
import assert from "node:assert/strict";
import { createAGUITransport } from "../dist/index.js";

// Every REST operation must carry auth and encode caller-supplied path segments.
test("REST paths, pagination, and stop body match SDK endpoints", async () => {
  const requests = [];
  const client = createAGUITransport({
    baseUrl: "https://chat.example/api/agui/",
    credentials: "include",
    headers: { Authorization: "Bearer token" },
    fetch: async (url, init) => {
      requests.push({ url, init });
      return Response.json(
        url.includes("/messages")
          ? { messages: [], sessionId: "session", nextCursor: "next" }
          : { threads: [] },
      );
    },
  });

  // Exercise each URL shape with characters that must never become path separators.
  const signal = new AbortController().signal;
  await client.listThreads("a/b", "group name", signal);
  const page = await client.loadMessages("a/b", "t/x", "cursor&1", signal);
  await client.stop("a/b", "t/x", "stream", signal);

  // Verify backend contracts without depending on implementation-specific helper calls.
  assert.equal(
    requests[0].url,
    "https://chat.example/api/agui/agents/a%2Fb/threads?group_id=group+name",
  );
  assert.match(
    requests[1].url,
    /threads\/t%2Fx\/messages\?limit=50&cursor=cursor%261$/,
  );
  assert.equal(page.sessionId, "session");
  assert.deepEqual(JSON.parse(requests[2].init.body), {
    threadId: "t/x",
    streamId: "stream",
  });
  for (const request of requests) {
    assert.equal(request.init.headers.get("Authorization"), "Bearer token");
    assert.equal(request.init.credentials, "include");
    assert.equal(request.init.signal, signal);
  }
});

// Unsupported list/feed capabilities should disable features instead of throwing parse errors.
test("501 responses report unsupported list and feed capabilities", async () => {
  const client = createAGUITransport({
    fetch: async () => new Response("unsupported", { status: 501 }),
  });
  const signal = new AbortController().signal;
  assert.equal((await client.listThreads("a", "g", signal)).supported, false);
  assert.equal(
    (await client.watchRuns("a", "cursor", signal)).supported,
    false,
  );
});

// Attachment uploads must let fetch generate their multipart boundary.
test("attachment uploads preserve session IDs without JSON content headers", async () => {
  const file = new File(["hello"], "hello.txt", { type: "text/plain" });
  const client = createAGUITransport({
    headers: { "Content-Type": "application/json" },
    fetch: async (_url, init) => {
      assert.equal(init.headers.has("Content-Type"), false);
      assert.equal(init.body.get("session_id"), "session");
      assert.equal(init.body.get("file").name, "hello.txt");
      return Response.json({ file_id: "file", filename: "hello.txt" });
    },
  });
  assert.equal(
    (await client.uploadAttachment(file, "session")).file_id,
    "file",
  );
});

import test from "node:test";
import assert from "node:assert/strict";
import { ChatController, StreamUnavailableError } from "../dist/index.js";
import { deferred, page, transport, until } from "./helpers.mjs";

// Late results from another conversation must never replace the active transcript.
test("conversation switching ignores stale history and stream events", async () => {
  const a = deferred();
  const controller = new ChatController({
    agent: "a",
    transport: transport({
      loadMessages: async (_agent, thread) =>
        thread === "old"
          ? a.promise
          : page([{ id: "new", role: "user", content: "new" }]),
    }),
  });
  const old = controller.selectThread("old");
  await controller.selectThread("new");
  a.resolve(page([{ id: "old", role: "user", content: "old" }]));
  await old;
  assert.equal(controller.getSnapshot().threadId, "new");
  assert.deepEqual(
    controller.getSnapshot().messages.map((message) => message.id),
    ["new"],
  );
});

// Fresh replay rebuilds assistant messages while preserving optimistic user input and history.
test("send, echo, tool calls, and completion produce one coherent transcript", async () => {
  let input;
  let lists = 0;
  const controller = new ChatController({
    agent: "a",
    createId: () => "id",
    transport: transport({
      listThreads: async () => {
        lists++;
        return {
          supported: true,
          threads: [{ thread_id: "id", title: "Hello" }],
        };
      },
      stream: async function* (_agent, _thread, value) {
        input = value;
        yield { type: "RUN_STARTED", runId: "run" };
        yield { type: "TEXT_MESSAGE_START", messageId: "msg_id", role: "user" };
        yield {
          type: "TEXT_MESSAGE_CONTENT",
          messageId: "msg_id",
          delta: "Hello",
        };
        yield { type: "TEXT_MESSAGE_END", messageId: "msg_id" };
        yield {
          type: "TEXT_MESSAGE_START",
          messageId: "answer",
          role: "assistant",
        };
        yield {
          type: "TEXT_MESSAGE_CONTENT",
          messageId: "answer",
          delta: "Hi",
        };
        yield {
          type: "TOOL_CALL_START",
          toolCallId: "call",
          parentMessageId: "answer",
          toolCallName: "search",
        };
        yield { type: "TOOL_CALL_ARGS", toolCallId: "call", delta: "{}" };
        yield {
          type: "TOOL_CALL_RESULT",
          messageId: "result",
          toolCallId: "call",
          content: "found",
        };
        yield { type: "RUN_FINISHED" };
      },
    }),
  });
  await controller.sendMessage("Hello");
  assert.equal(input.messages.length, 1);
  assert.equal(controller.getSnapshot().messages[0].content, "Hello");
  assert.equal(
    controller.getSnapshot().messages[1].toolCalls[0].function.arguments,
    "{}",
  );
  assert.equal(controller.getSnapshot().messages[2].toolCallId, "call");
  assert.equal(controller.getSnapshot().isRunning, false);
  assert.ok(lists >= 2);
});

// Pagination must merge against the latest streamed messages rather than its stale request snapshot.
test("pagination preserves live content and removes overlap", async () => {
  const older = deferred();
  const live = deferred();
  const controller = new ChatController({
    agent: "a",
    transport: transport({
      loadMessages: async (_agent, _thread, cursor) =>
        cursor
          ? older.promise
          : page([{ id: "m", role: "assistant", content: "old" }], "page2"),
      stream: async function* () {
        yield { type: "TEXT_MESSAGE_START", messageId: "m", role: "assistant" };
        yield { type: "TEXT_MESSAGE_CONTENT", messageId: "m", delta: "live" };
        await live.promise;
        yield { type: "RUN_FINISHED" };
      },
    }),
  });
  await controller.selectThread("thread");
  await until(() => controller.getSnapshot().messages[0].content === "live");
  const loading = controller.loadOlderMessages();
  older.resolve(
    page([
      { id: "older", role: "user", content: "hello" },
      { id: "m", role: "assistant", content: "stale" },
    ]),
  );
  await loading;
  assert.deepEqual(
    controller.getSnapshot().messages.map((message) => message.content),
    ["hello", "live"],
  );
  live.resolve();
  await until(() => !controller.getSnapshot().isRunning);
});

// Stop requests target the displayed stream and do not abort its final response.
test("stop cancels server work without disconnecting the event stream", async () => {
  const finish = deferred();
  let streamSignal, stopped;
  const controller = new ChatController({
    agent: "a",
    transport: transport({
      stream: async function* (_agent, _thread, _input, options) {
        streamSignal = options.signal;
        yield {
          type: "CUSTOM",
          name: "hastekit.stream_id",
          value: { streamId: "stream-id" },
        };
        await finish.promise;
        yield { type: "RUN_FINISHED" };
      },
      stop: async (...args) => {
        stopped = args;
      },
    }),
  });
  const sending = controller.sendMessage("hello");
  await until(() => controller.getSnapshot().lastEvent);
  await controller.stop();
  assert.equal(stopped[2], "stream-id");
  assert.equal(streamSignal.aborted, false);
  finish.resolve();
  await sending;
});

// Replay expiry falls back to persisted history rather than leaving a truncated reply.
test("expired stream reloads history", async () => {
  const controller = new ChatController({
    agent: "a",
    transport: transport({
      loadMessages: async () =>
        page([{ id: "done", role: "assistant", content: "complete" }]),
      stream: async function* () {
        throw new StreamUnavailableError();
      },
    }),
  });
  await controller.sendMessage("hello");
  assert.equal(controller.getSnapshot().messages.at(-1).content, "complete");
  assert.equal(controller.getSnapshot().error, null);
});

// Unsupported feeds must terminate observation instead of creating a busy loop.
test("unsupported run feed is requested only once", async () => {
  let watches = 0;
  const controller = new ChatController({
    agent: "a",
    transport: transport({
      watchRuns: async () => {
        watches++;
        return { supported: false, events: [], cursor: "" };
      },
    }),
  });
  const unmount = controller.mount();
  await until(() => watches === 1);
  await new Promise((resolve) => setTimeout(resolve, 10));
  unmount();
  assert.equal(watches, 1);
});

// A delayed event from a detached stream must not write into the next conversation.
test("late stream events cannot cross a conversation switch", async () => {
  const release = deferred();
  const controller = new ChatController({
    agent: "a",
    transport: transport({
      loadMessages: async () =>
        page([{ id: "new", role: "user", content: "new" }]),
      stream: async function* (_agent, _thread, input) {
        if (!input) return;
        await release.promise;
        yield {
          type: "TEXT_MESSAGE_CONTENT",
          messageId: "stale",
          delta: "old answer",
        };
      },
    }),
  });
  const sending = controller.sendMessage("old question");
  await controller.selectThread("new-thread");
  release.resolve();
  await sending;
  assert.deepEqual(
    controller.getSnapshot().messages.map((message) => message.id),
    ["new"],
  );
});

// StrictMode-style cleanup and setup must abandon the first pending history request.
test("effect remount restores selection without accepting abandoned requests", async () => {
  const first = deferred();
  let loads = 0;
  const controller = new ChatController({
    agent: "a",
    initialThreadId: "thread",
    transport: transport({
      loadMessages: async () =>
        ++loads === 1
          ? first.promise
          : page([{ id: "restored", role: "assistant", content: "restored" }]),
    }),
  });
  controller.mount()();
  const cleanup = controller.mount();
  await until(() => controller.getSnapshot().messages[0]?.id === "restored");
  first.resolve(page([{ id: "abandoned", role: "assistant" }]));
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(controller.getSnapshot().messages[0].id, "restored");
  cleanup();
});

// A failed history fallback must remain visible to applications that auto-connect.
test("history recovery failures surface in chat state", async () => {
  const controller = new ChatController({
    agent: "a",
    transport: transport({
      loadMessages: async () => {
        throw new Error("history unavailable");
      },
      stream: async function* () {
        throw new StreamUnavailableError();
      },
    }),
  });
  await assert.rejects(controller.sendMessage("hello"), /history unavailable/);
  assert.equal(controller.getSnapshot().error.message, "history unavailable");
  assert.equal(controller.getSnapshot().connection, "idle");
});

// A run notice can precede broker publication, so the following join must allow waiting.
test("run-feed starts rejoin with publication wait and refresh sidebar", async () => {
  const notice = deferred();
  const attached = deferred();
  let watching = 0;
  let joinedWithWait = false;
  const controller = new ChatController({
    agent: "a",
    transport: transport({
      watchRuns: async (_agent, _cursor, signal) => {
        watching++;
        if (watching === 1) return notice.promise;
        await new Promise((resolve) =>
          signal.addEventListener("abort", resolve, { once: true }),
        );
        return { events: [], cursor: "1", supported: false };
      },
      stream: async function* (_agent, _thread, _input, options) {
        if (options.waitForRun) {
          joinedWithWait = true;
          await attached.promise;
          yield { type: "RUN_FINISHED" };
        }
      },
    }),
  });
  const cleanup = controller.mount();
  await controller.selectThread("thread");
  await until(() => controller.getSnapshot().connection === "idle");
  notice.resolve({
    supported: true,
    cursor: "1",
    events: [{ event: "RUN_STARTED", threadId: "thread", streamId: "stream" }],
  });
  await until(() => joinedWithWait);
  assert.ok(controller.getSnapshot().activeThreadIds.includes("thread"));
  attached.resolve();
  await until(() => controller.getSnapshot().connection === "idle");
  cleanup();
});

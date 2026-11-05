import test from "node:test";
import assert from "node:assert/strict";
import { ChatController } from "../dist/index.js";
import { deferred, page, transport, until } from "./helpers.mjs";

// A stream the test feeds one event at a time.
function feed() {
  const events = [];
  let wake = () => {};
  return {
    push(...items) {
      events.push(...items);
      wake();
    },
    async *read() {
      for (;;) {
        while (!events.length)
          await new Promise((resolve) => {
            wake = resolve;
          });
        const event = events.shift();
        yield event;
        if (event.type === "RUN_FINISHED" || event.type === "RUN_ERROR") return;
      }
    },
  };
}

// Record every stream opened and every turn steered, with steering overridable.
function fixture(steer = async () => true, overrides = {}) {
  let ids = 0;
  const streams = [];
  const steers = [];
  const controller = new ChatController({
    agent: "a",
    createId: () => `id${++ids}`,
    transport: transport({
      stream: (_agent, _thread, input, options) => {
        const stream = feed();
        streams.push({ input, options, push: stream.push });
        return stream.read();
      },
      steer: (_agent, _thread, input) => {
        steers.push(input);
        return steer(input);
      },
      ...overrides,
    }),
  });
  return { controller, streams, steers };
}

// Start a run this client is following.
async function running({ controller, streams }) {
  void controller.sendMessage("first").catch(() => {});
  await until(() => streams.length === 1);
  streams[0].push({ type: "RUN_STARTED", runId: "run" });
  await until(() => controller.getSnapshot().run?.status === "running");
}

// The three events a run publishes when it takes a text turn in.
function announce(id, content) {
  return [
    { type: "TEXT_MESSAGE_START", messageId: id, role: "user" },
    { type: "TEXT_MESSAGE_CONTENT", messageId: id, delta: content },
    { type: "TEXT_MESSAGE_END", messageId: id },
  ];
}

const contents = (controller) =>
  controller.getSnapshot().messages.map((message) => message.content);

test("a steered turn posts only itself, shows at once, and keeps the stream", async () => {
  const f = fixture();
  await running(f);
  await f.controller.steer("also check the logs");

  assert.equal(f.streams.length, 1);
  assert.deepEqual(
    f.steers[0].messages.map((message) => message.content),
    ["also check the logs"],
  );
  assert.deepEqual(contents(f.controller), ["first", "also check the logs"]);
  const id = f.steers[0].messages[0].id;
  assert.deepEqual(f.controller.getSnapshot().queuedMessageIds, [id]);
  assert.equal(f.controller.getSnapshot().isRunning, true);
});

test("a queued turn lands where the run takes it in, without doubling its text", async () => {
  const f = fixture();
  await running(f);
  f.streams[0].push(
    {
      type: "TOOL_CALL_START",
      toolCallId: "c1",
      parentMessageId: "a1",
      toolCallName: "search",
    },
    { type: "TOOL_CALL_END", toolCallId: "c1" },
  );
  await until(() => f.controller.getSnapshot().messages.length === 2);
  await f.controller.steer("hurry up");
  const id = f.steers[0].messages[0].id;

  // Work finishing after the turn was sent, but before the run saw it, goes first.
  f.streams[0].push({
    type: "TOOL_CALL_RESULT",
    messageId: "r1",
    toolCallId: "c1",
    content: "found",
  });
  await until(() => f.controller.getSnapshot().messages.length === 4);
  const order = () => f.controller.getSnapshot().messages.map((m) => m.id);
  assert.deepEqual(order().slice(1), ["a1", "r1", id]);

  f.streams[0].push(...announce(id, "hurry up"), {
    type: "TEXT_MESSAGE_START",
    messageId: "a2",
    role: "assistant",
  });
  await until(() => f.controller.getSnapshot().messages.length === 5);
  assert.deepEqual(order().slice(1), ["a1", "r1", id, "a2"]);
  assert.deepEqual(f.controller.getSnapshot().queuedMessageIds, []);
  assert.equal(
    f.controller.getSnapshot().messages.find((m) => m.id === id).content,
    "hurry up",
  );

  // Taken in, so its end needs no follow-up.
  f.streams[0].push({ type: "RUN_FINISHED" });
  await until(() => !f.controller.getSnapshot().isRunning);
  await new Promise((resolve) => setTimeout(resolve, 5));
  assert.equal(f.streams.length, 1);
  assert.equal(f.steers.length, 1);
});

test("a multipart turn is settled by the server's copy", async () => {
  const f = fixture();
  await running(f);
  const parts = [{ type: "text", text: "see this" }];
  await f.controller.steer(parts);
  const id = f.steers[0].messages[0].id;
  const stored = [{ type: "image", source: { value: "attachment://f" } }];
  f.streams[0].push({
    type: "CUSTOM",
    name: "input_message",
    value: { id, role: "user", content: stored },
  });
  await until(() => !f.controller.getSnapshot().queuedMessageIds.length);
  assert.deepEqual(contents(f.controller), ["first", stored]);
});

test("a turn that cannot be sent is withdrawn and reported", async () => {
  const f = fixture(async () => {
    throw new Error("HTTP 409");
  });
  await running(f);
  await assert.rejects(f.controller.steer("again"), /409/);
  assert.deepEqual(contents(f.controller), ["first"]);
  assert.deepEqual(f.controller.getSnapshot().queuedMessageIds, []);
  assert.match(f.controller.getSnapshot().error.message, /409/);
  assert.equal(f.streams.length, 1);
});

// A 204 only says a turn reached the queue. One landing as the run finishes, after
// its last look at the queue, is acknowledged and then cleared by the next run.
test("a turn the finished run never took in goes out as the next turn", async () => {
  const f = fixture();
  await running(f);
  await f.controller.steer("one more thing");
  const id = f.steers[0].messages[0].id;
  f.streams[0].push(
    { type: "TEXT_MESSAGE_START", messageId: "a1", role: "assistant" },
    { type: "TEXT_MESSAGE_CONTENT", messageId: "a1", delta: "done" },
    { type: "RUN_FINISHED" },
  );
  await until(() => f.streams.length === 2);
  assert.deepEqual(f.streams[1].input.messages, [
    { id, role: "user", content: "one more thing" },
  ]);
  assert.deepEqual(f.controller.getSnapshot().queuedMessageIds, []);
  // Now behind the reply it came after.
  assert.deepEqual(contents(f.controller), ["first", "done", "one more thing"]);
  assert.equal(f.controller.getSnapshot().isRunning, true);

  // The new run echoes it like any turn, and does not send it again.
  f.streams[1].push(...announce(id, "one more thing"), {
    type: "RUN_FINISHED",
  });
  await until(() => !f.controller.getSnapshot().isRunning);
  await new Promise((resolve) => setTimeout(resolve, 5));
  assert.equal(f.streams.length, 2);
  assert.deepEqual(contents(f.controller), ["first", "done", "one more thing"]);
});

test("a turn that started its own run is joined once the old stream ends", async () => {
  const f = fixture(async () => false);
  await running(f);
  await f.controller.steer("too late");
  assert.deepEqual(f.controller.getSnapshot().queuedMessageIds, []);
  assert.equal(f.streams.length, 1);

  f.streams[0].push({ type: "RUN_FINISHED" });
  await until(() => f.streams.length === 2);
  assert.equal(f.streams[1].input, undefined);
  assert.equal(f.streams[1].options.waitForRun, true);
});

test("an unanswered POST is waited for rather than sent twice", async () => {
  const answer = deferred();
  const f = fixture(() => answer.promise);
  await running(f);
  const steering = f.controller.steer("racing the end");
  f.streams[0].push({ type: "RUN_FINISHED" });
  await until(() => !f.controller.getSnapshot().isRunning);
  await new Promise((resolve) => setTimeout(resolve, 5));
  assert.equal(f.streams.length, 1);

  // A run took it, but none is being followed: see whether it announces it.
  answer.resolve(true);
  await steering;
  await until(() => f.streams.length === 2);
  assert.equal(f.streams[1].input, undefined);
  assert.equal(f.streams[1].options.waitForRun, true);

  // It does not, so that run is over and the turn goes out once more.
  f.streams[1].push({ type: "RUN_FINISHED" });
  await until(() => f.streams.length === 3);
  assert.equal(f.streams[2].input.messages[0].content, "racing the end");
  assert.equal(f.steers.length, 1);
});

test("stopping discards queued turns instead of starting another run", async () => {
  const f = fixture();
  await running(f);
  await f.controller.steer("never mind");
  await f.controller.stop();
  assert.deepEqual(contents(f.controller), ["first"]);
  assert.deepEqual(f.controller.getSnapshot().queuedMessageIds, []);
  f.streams[0].push({ type: "RUN_FINISHED" });
  await until(() => !f.controller.getSnapshot().isRunning);
  await new Promise((resolve) => setTimeout(resolve, 5));
  assert.equal(f.streams.length, 1);
});

test("a failed run discards queued turns and keeps its error", async () => {
  const f = fixture();
  await running(f);
  await f.controller.steer("and this");
  f.streams[0].push({ type: "RUN_ERROR", message: "model down" });
  await until(() => !f.controller.getSnapshot().queuedMessageIds.length);
  assert.deepEqual(contents(f.controller), ["first"]);
  assert.equal(f.controller.getSnapshot().error.message, "model down");
  assert.equal(f.streams.length, 1);
});

test("a paused run keeps its queue for when it resumes", async () => {
  const f = fixture();
  await running(f);
  await f.controller.steer("when you can");
  const id = f.steers[0].messages[0].id;
  const interrupt = { id: "c1", reason: "tool_call", toolCallId: "c1" };
  f.streams[0].push({
    type: "RUN_FINISHED",
    outcome: { type: "interrupt", interrupts: [interrupt] },
  });
  await until(() => f.controller.getSnapshot().run?.status === "paused");
  await new Promise((resolve) => setTimeout(resolve, 5));
  assert.equal(f.streams.length, 1);
  assert.deepEqual(f.controller.getSnapshot().queuedMessageIds, [id]);

  void f.controller
    .resume([{ interruptId: "c1", status: "resolved" }])
    .catch(() => {});
  await until(() => f.streams.length === 2);
  f.streams[1].push(...announce(id, "when you can"), { type: "RUN_FINISHED" });
  await until(() => !f.controller.getSnapshot().isRunning);
  await new Promise((resolve) => setTimeout(resolve, 5));
  assert.deepEqual(f.controller.getSnapshot().queuedMessageIds, []);
  assert.equal(f.streams.length, 2);
});

test("with no stream attached it is an ordinary send", async () => {
  const f = fixture();
  f.controller.newThread();
  const sending = f.controller.steer("hello");
  await until(() => f.streams.length === 1);
  assert.equal(f.streams[0].input.messages[0].content, "hello");
  assert.equal(f.steers.length, 0);
  assert.deepEqual(f.controller.getSnapshot().queuedMessageIds, []);
  f.streams[0].push({ type: "RUN_FINISHED" });
  await sending;
});

test("empty turns and transports without steering are rejected up front", async () => {
  const f = fixture();
  await running(f);
  await assert.rejects(f.controller.steer("   "), /content is required/);
  await assert.rejects(f.controller.steer([]), /content is required/);
  assert.equal(f.steers.length, 0);

  const bare = fixture(undefined, { steer: undefined });
  await running(bare);
  await assert.rejects(bare.controller.steer("hi"), /not supported/);
  assert.deepEqual(contents(bare.controller), ["first"]);
});

test("a late answer cannot disturb the conversation the user moved to", async () => {
  const answer = deferred();
  const f = fixture(() => answer.promise, {
    loadMessages: async () =>
      page([{ id: "other", role: "user", content: "x" }]),
  });
  await running(f);
  const steering = f.controller.steer("still deliver me");
  await f.controller.selectThread("other");
  assert.deepEqual(f.controller.getSnapshot().queuedMessageIds, []);
  const opened = f.streams.length;

  answer.resolve(false);
  await steering;
  await new Promise((resolve) => setTimeout(resolve, 5));
  assert.deepEqual(contents(f.controller), ["x"]);
  assert.equal(f.streams.length, opened);
});

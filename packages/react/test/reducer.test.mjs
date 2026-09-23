import test from "node:test";
import assert from "node:assert/strict";
import { ChatController } from "../dist/index.js";
import { EventReducer } from "../dist/reducer.js";
import { transport } from "./helpers.mjs";

// Build an isolated reducer fixture without starting network observation.
function fixture() {
  const controller = new ChatController({ agent: "a", transport: transport() });
  let snapshot = controller.getSnapshot();
  const reducer = new EventReducer();
  return {
    apply(event) {
      snapshot = { ...snapshot, ...reducer.apply(snapshot, event) };
      return snapshot;
    },
  };
}

// Approval state survives the terminal event and retains the server's projected shape.
test("interrupts and compaction are exposed as renderable state", () => {
  const reducer = fixture();
  reducer.apply({ type: "RUN_STARTED", runId: "run" });
  assert.equal(
    reducer.apply({ type: "CUSTOM", name: "hastekit.summarization_started" })
      .isCompacting,
    true,
  );
  assert.equal(
    reducer.apply({ type: "CUSTOM", name: "hastekit.summarization_completed" })
      .isCompacting,
    false,
  );
  const interrupts = [{ toolCallId: "call", reason: "approval" }];
  reducer.apply({
    type: "CUSTOM",
    name: "on_interrupt",
    value: { runId: "run", interrupts, pendingToolCalls: [{ id: "call" }] },
  });
  const snapshot = reducer.apply({ type: "RUN_FINISHED" });
  assert.equal(snapshot.run.awaitingApproval, true);
  assert.deepEqual(snapshot.run.interrupts, interrupts);
  assert.equal(snapshot.run.pendingToolCalls[0].id, "call");
});

// JSON patch updates must be immutable and reject prototype mutation paths.
test("agent state patches preserve previous snapshots", () => {
  const reducer = fixture();
  const original = { count: 1 };
  reducer.apply({ type: "STATE_SNAPSHOT", snapshot: original });
  const snapshot = reducer.apply({
    type: "STATE_DELTA",
    delta: [{ op: "replace", path: "/count", value: 2 }],
  });
  assert.equal(snapshot.state.count, 2);
  assert.equal(original.count, 1);
  assert.throws(() =>
    reducer.apply({
      type: "STATE_DELTA",
      delta: [{ op: "add", path: "/__proto__/unsafe", value: true }],
    }),
  );
});

// Compact event variants initialize their message once and continue subsequent deltas.
test("compact text and tool chunks accumulate without duplicate messages", () => {
  const reducer = fixture();
  reducer.apply({
    type: "TEXT_MESSAGE_CHUNK",
    messageId: "m",
    delta: "hello ",
  });
  reducer.apply({ type: "TEXT_MESSAGE_CHUNK", delta: "world" });
  reducer.apply({
    type: "TOOL_CALL_CHUNK",
    toolCallId: "c",
    parentMessageId: "m",
    toolCallName: "search",
    delta: "{",
  });
  const snapshot = reducer.apply({ type: "TOOL_CALL_CHUNK", delta: "}" });
  assert.equal(snapshot.messages.length, 1);
  assert.equal(snapshot.messages[0].content, "hello world");
  assert.equal(snapshot.messages[0].toolCalls[0].function.arguments, "{}");
});

// Elicitation pauses need to survive completion even when no approval is requested.
test("elicitation interrupts survive without marking them as approvals", () => {
  const reducer = fixture();
  reducer.apply({
    type: "CUSTOM",
    name: "on_interrupt",
    value: { interrupts: [{ kind: "elicitation" }], pendingToolCalls: [] },
  });
  const snapshot = reducer.apply({ type: "RUN_FINISHED" });
  assert.equal(snapshot.run.awaitingApproval, false);
  assert.equal(snapshot.run.interrupts[0].kind, "elicitation");
});

// Persisted carriers and streamed results use different message IDs for one tool execution.
test("replay reuses persisted tool identities without moving the call after the answer", () => {
  const reducer = fixture();
  reducer.apply({
    type: "MESSAGES_SNAPSHOT",
    messages: [
      {
        id: "history-call",
        role: "assistant",
        toolCalls: [
          {
            id: "call",
            type: "function",
            function: { name: "bash", arguments: '{"code":"date"}' },
          },
        ],
      },
      {
        id: "history-result",
        role: "tool",
        toolCallId: "call",
        content: "today",
      },
      { id: "answer", role: "assistant", content: "Today" },
    ],
  });
  reducer.apply({
    type: "TOOL_CALL_START",
    toolCallId: "call",
    toolCallName: "bash",
  });
  reducer.apply({
    type: "TOOL_CALL_ARGS",
    toolCallId: "call",
    delta: '{"code":"date"}',
  });
  const state = reducer.apply({
    type: "TOOL_CALL_RESULT",
    messageId: "stream-result",
    toolCallId: "call",
    content: "today",
  });
  assert.deepEqual(
    state.messages.map((message) => message.id),
    ["history-call", "history-result", "answer"],
  );
  assert.equal(
    state.messages[0].toolCalls[0].function.arguments,
    '{"code":"date"}',
  );
});

// Canonical snapshots replace synthetic tool messages while retaining older paginated history.
test("snapshot reconciles synthetic calls and outputs by call identity", () => {
  const reducer = fixture();
  reducer.apply({ type: "TEXT_MESSAGE_START", messageId: "old", role: "user" });
  reducer.apply({
    type: "TOOL_CALL_START",
    toolCallId: "call",
    toolCallName: "bash",
  });
  reducer.apply({
    type: "TOOL_CALL_RESULT",
    toolCallId: "call",
    content: "today",
  });
  const state = reducer.apply({
    type: "MESSAGES_SNAPSHOT",
    messages: [
      {
        id: "persisted-call",
        role: "assistant",
        toolCalls: [
          {
            id: "call",
            type: "function",
            function: { name: "bash", arguments: "{}" },
          },
        ],
      },
      {
        id: "persisted-result",
        role: "tool",
        toolCallId: "call",
        content: "today",
      },
      { id: "answer", role: "assistant", content: "Today" },
    ],
  });
  assert.deepEqual(
    state.messages.map((message) => message.id),
    ["old", "persisted-call", "persisted-result", "answer"],
  );
});

import test from "node:test";
import assert from "node:assert/strict";
import { ChatController, createAGUITransport } from "../dist/index.js";
import { deferred, transport, until } from "./helpers.mjs";

const toolCall = [
  { type: "RUN_STARTED", threadId: "t", runId: "r1" },
  { type: "TOOL_CALL_START", toolCallId: "c1", toolCallName: "get_selection" },
  { type: "TOOL_CALL_ARGS", toolCallId: "c1", delta: '{"max":3}' },
  { type: "TOOL_CALL_END", toolCallId: "c1" },
];

function selectionTool(calls) {
  return {
    name: "get_selection",
    description: "Read the selected text",
    handler: async (args, context) => {
      calls.push([args, context.toolCallId, context.threadId]);
      return { text: "selected" };
    },
  };
}

// Interrupt mode: the run pauses on the call and the controller resumes it with the result.
test("a pause on a client tool resumes with the handler's result", async () => {
  const inputs = [];
  const calls = [];
  const controller = new ChatController({
    agent: "a",
    createId: () => "id",
    clientTools: [selectionTool(calls)],
    transport: transport({
      stream: async function* (_agent, _thread, input) {
        inputs.push(input);
        if (inputs.length === 1) {
          yield* toolCall;
          // The call is left unanswered: the client works out it is pending.
          yield { type: "RUN_FINISHED", outcome: { type: "success" } };
          return;
        }
        yield { type: "RUN_STARTED", threadId: "t", runId: "r2" };
        yield {
          type: "TOOL_CALL_RESULT",
          messageId: "m",
          toolCallId: "c1",
          content: '{"text":"selected"}',
        };
        yield { type: "RUN_FINISHED", outcome: { type: "success" } };
      },
    }),
  });
  await controller.sendMessage("summarize my selection");

  assert.deepEqual(inputs[0].tools, [
    {
      name: "get_selection",
      description: "Read the selected text",
      parameters: { type: "object", properties: {} },
    },
  ]);
  assert.deepEqual(calls, [
    [{ max: 3 }, "c1", controller.getSnapshot().threadId],
  ]);
  assert.equal(inputs.length, 2);
  // The resuming turn is the result as a tool message, with the tools again.
  assert.deepEqual(
    inputs[1].messages.map((m) => [m.role, m.toolCallId, m.content]),
    [["tool", "c1", '{"text":"selected"}']],
  );
  assert.equal(inputs[1].tools.length, 1);
  assert.equal(controller.getSnapshot().isRunning, false);
});

// Wait mode: the server answers on the same run, so there is no second run.
test("a waiting run takes the result sent while it streams, without resuming", async () => {
  const inputs = [];
  const sent = [];
  const controller = new ChatController({
    agent: "a",
    createId: () => "id",
    clientTools: [selectionTool([])],
    transport: transport({
      sendToolResults: async (_agent, _thread, input) => {
        sent.push(input);
      },
      stream: async function* (_agent, _thread, input) {
        inputs.push(input);
        yield* toolCall;
        await new Promise((resolve) => setTimeout(resolve, 10));
        yield {
          type: "TOOL_CALL_RESULT",
          messageId: "m",
          toolCallId: "c1",
          content: "selected",
        };
        yield { type: "RUN_FINISHED", outcome: { type: "success" } };
      },
    }),
  });
  await controller.sendMessage("go");
  assert.equal(inputs.length, 1);
  assert.equal(sent.length, 1);
  assert.deepEqual(
    sent[0].messages.map((m) => [m.role, m.toolCallId, m.content]),
    [["tool", "c1", '{"text":"selected"}']],
  );
});

// A reconnect replays events; a call the replay shows answered never runs again.
test("replayed tool calls that were answered never run the handler", async () => {
  const calls = [];
  const controller = new ChatController({
    agent: "a",
    createId: () => "id",
    initialThreadId: "t",
    clientTools: [selectionTool(calls)],
    transport: transport({
      stream: async function* () {
        yield* toolCall;
        yield { type: "TOOL_CALL_RESULT", messageId: "m", toolCallId: "c1", content: "earlier" };
        yield { type: "RUN_FINISHED", outcome: { type: "success" } };
      },
    }),
  });
  controller.newThread();
  await controller.connect();
  assert.equal(calls.length, 0);
});

// A joined run that ends waiting on this client's tool — its page went away
// before answering — is answered here, once.
test("a joined run left waiting on a client tool is answered once", async () => {
  const calls = [];
  const inputs = [];
  const controller = new ChatController({
    agent: "a",
    createId: () => "id",
    initialThreadId: "t",
    clientTools: [selectionTool(calls)],
    transport: transport({
      stream: async function* (_agent, _thread, input) {
        inputs.push(input);
        // Whatever is posted, the run keeps pausing on the call: not answered in a loop.
        yield* toolCall;
        yield { type: "RUN_FINISHED", outcome: { type: "success" } };
      },
    }),
  });
  controller.newThread();
  await controller.connect();
  assert.equal(calls.length, 1);
  assert.deepEqual(
    inputs.filter(Boolean).map((input) => input.messages.map((m) => [m.role, m.toolCallId])),
    [[["tool", "c1"]]],
  );
});

test("tool results are sent as a turn on the agent's run endpoint", async () => {
  const requests = [];
  const client = createAGUITransport({
    fetch: async (url, init) => {
      requests.push([init.method, url, init.body]);
      return new Response(null, { status: 204 });
    },
  });
  const input = {
    threadId: "t 1",
    runId: "r",
    messages: [{ id: "m", role: "tool", toolCallId: "c1", content: "x" }],
    state: {},
    tools: [],
    context: [],
    forwardedProps: {},
  };
  await client.sendToolResults("a/b", "t 1", input);
  assert.deepEqual(requests, [
    ["POST", "/api/agui/agents/a%2Fb/run", JSON.stringify(input)],
  ]);
});

// A tool whose handler finishes only when the test says so.
function slowTool(calls, done) {
  return {
    name: "get_selection",
    handler: async (args, context) => {
      calls.push([args, context.toolCallId, context.threadId]);
      await done.promise;
      return "selected";
    },
  };
}

// Tool messages sent through the fire-and-forget path, by thread.
function sentResults(sent) {
  return async (_agent, thread, input) => {
    sent.push([thread, input.threadId, input.messages.map((m) => [m.role, m.toolCallId, m.content])]);
  };
}

test("a result that finishes after the user sent another message is still sent", async () => {
  const done = deferred();
  const sent = [];
  let runs = 0;
  const controller = new ChatController({
    agent: "a",
    createId: () => "id",
    clientTools: [slowTool([], done)],
    transport: transport({
      sendToolResults: sentResults(sent),
      stream: async function* () {
        runs++;
        if (runs === 1) yield* toolCall;
        yield { type: "RUN_FINISHED", outcome: { type: "success" } };
      },
    }),
  });
  const first = controller.sendMessage("summarize");
  // The run paused on the tool and the composer is free again; the user
  // carries on before the tool finishes.
  await until(() => {
    const snapshot = controller.getSnapshot();
    return !snapshot.isRunning && snapshot.run?.status === "paused";
  });
  await controller.sendMessage("never mind");
  done.resolve();
  await first;
  await until(() => sent.length > 0);
  assert.deepEqual(sent[0][2], [["tool", "c1", "selected"]]);
});

test("a mixed pause still gets the client tool's result", async () => {
  const done = deferred();
  const sent = [];
  const controller = new ChatController({
    agent: "a",
    createId: () => "id",
    clientTools: [slowTool([], done)],
    transport: transport({
      sendToolResults: sentResults(sent),
      stream: async function* () {
        yield* toolCall;
        // The run waits on a person for another call; the client tool is left unanswered.
        yield {
          type: "RUN_FINISHED",
          outcome: { type: "interrupt", interrupts: [{ id: "c2", reason: "tool_call", toolCallId: "c2" }] },
        };
      },
    }),
  });
  await controller.sendMessage("go");
  done.resolve();
  await until(() => sent.length > 0);
  assert.deepEqual(sent[0][2], [["tool", "c1", "selected"]]);
});

test("a turn folded into another run does not re-run that run's tools", async () => {
  const calls = [];
  const controller = new ChatController({
    agent: "a",
    createId: () => "id",
    clientTools: [selectionTool(calls)],
    transport: transport({
      sendToolResults: async () => {},
      stream: async function* (_agent, _thread, _input, options) {
        // The POST joined a run another tab started; its replay follows.
        options.onFolded?.();
        yield* toolCall;
        yield { type: "TOOL_CALL_RESULT", messageId: "m", toolCallId: "c1", content: "answered there" };
        yield { type: "RUN_FINISHED", outcome: { type: "success" } };
      },
    }),
  });
  await controller.sendMessage("hi");
  assert.equal(calls.length, 0);
});

test("a result is sent to its own conversation after the user switched away", async () => {
  const done = deferred();
  const sent = [];
  const controller = new ChatController({
    agent: "a",
    createId: () => "id",
    clientTools: [slowTool([], done)],
    transport: transport({
      sendToolResults: sentResults(sent),
      stream: async function* (_agent, thread, input) {
        if (thread === "t1" && input) {
          yield* toolCall;
          // Also waiting on a person, so the turn ends without waiting on the tool.
          yield {
            type: "RUN_FINISHED",
            outcome: { type: "interrupt", interrupts: [{ id: "c2", reason: "tool_call", toolCallId: "c2" }] },
          };
        }
      },
    }),
  });
  await controller.selectThread("t1");
  await controller.sendMessage("go");
  await controller.selectThread("t2");
  done.resolve();
  await until(() => sent.length > 0);
  assert.equal(sent[0][0], "t1");
  assert.equal(sent[0][1], "t1");
});

test("opening a thread paused on a client tool answers it", async () => {
  const calls = [];
  const inputs = [];
  const controller = new ChatController({
    agent: "a",
    createId: () => "id",
    clientTools: [selectionTool(calls)],
    transport: transport({
      loadMessages: async () => ({
        messages: [
          { id: "u1", role: "user", content: "summarize" },
          {
            id: "a1",
            role: "assistant",
            toolCalls: [{ id: "c1", type: "function", function: { name: "get_selection", arguments: '{"max":3}' } }],
          },
        ],
        run: { status: "paused", awaitingApproval: false, pendingToolCallIds: ["c1"] },
        nextCursor: "",
        sessionId: "t",
      }),
      stream: async function* (_agent, _thread, input) {
        inputs.push(input);
        if (input) yield { type: "RUN_FINISHED", outcome: { type: "success" } };
      },
    }),
  });
  await controller.selectThread("t");
  await until(() => inputs.some(Boolean));
  assert.deepEqual(calls.map((c) => c[1]), ["c1"]);
  assert.deepEqual(
    inputs.find(Boolean).messages.map((m) => [m.role, m.toolCallId]),
    [["tool", "c1"]],
  );
});

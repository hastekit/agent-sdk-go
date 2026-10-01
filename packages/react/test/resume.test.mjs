import test from "node:test";
import assert from "node:assert/strict";
import { ChatController } from "../dist/index.js";
import { transport } from "./helpers.mjs";

// Interrupts are answered with AG-UI 1.0 resume entries, not forwardedProps.
test("resume sends the spec's resume entries without a new turn", async () => {
  let input;
  const controller = new ChatController({
    agent: "a",
    createId: () => "id",
    initialThreadId: "t",
    transport: transport({
      stream: async function* (_agent, _thread, value) {
        input = value;
        yield { type: "RUN_FINISHED", outcome: { type: "success" } };
      },
    }),
  });
  controller.newThread();
  const entries = [
    { interruptId: "call_1", status: "resolved", payload: { approved: true } },
    { interruptId: "call_2", status: "cancelled" },
  ];
  await controller.resume(entries);
  assert.deepEqual(input.resume, entries);
  assert.deepEqual(input.messages, []);
  assert.equal(input.forwardedProps.command, undefined);
  await assert.rejects(controller.resume([]), /interrupt answer/);
});

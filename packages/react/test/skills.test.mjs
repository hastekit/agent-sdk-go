import test from "node:test";
import assert from "node:assert/strict";
import { ChatController, createAGUITransport } from "../dist/index.js";
import { deferred, transport } from "./helpers.mjs";

// Preserve authentication, cancellation, and encoded agent identifiers for catalog requests.
test("skill catalog uses the agent endpoint", async () => {
  const signal = new AbortController().signal;
  const client = createAGUITransport({
    headers: { Authorization: "Bearer test" },
    fetch: async (url, init) => {
      assert.equal(url, "/api/agui/agents/a%2Fb/skills");
      assert.equal(init.signal, signal);
      assert.equal(init.headers.get("Authorization"), "Bearer test");
      return Response.json({ skills: [{ name: "review", enabled: false }] });
    },
  });
  assert.equal((await client.listSkills("a/b", signal))[0].name, "review");
});

// Required policy and optional choices must survive refresh and reach the outgoing run.
test("skill choices respect policy and serialize per run", async () => {
  let input;
  let catalog = [
    { name: "required", required: true, enabled: true },
    { name: "default", enabled: true },
    { name: "optional", enabled: false },
  ];
  const controller = new ChatController({
    agent: "a",
    createId: () => "id",
    forwardedProps: { tenant: "test" },
    transport: transport({
      listSkills: async () => catalog,
      stream: async function* (_agent, _thread, value) {
        input = value;
        yield { type: "RUN_FINISHED" };
      },
    }),
  });
  await controller.refreshSkills();
  assert.throws(
    () => controller.setSkillEnabled("required", false),
    /required/,
  );
  controller.setSkillEnabled("default", false);
  controller.setSkillEnabled("optional", true);
  await controller.refreshSkills();
  await controller.sendMessage("hello");
  assert.deepEqual(input.forwardedProps, {
    tenant: "test",
    skills: { enable: ["optional"], disable: ["default"] },
  });

  // A changed catalog removes stale choices and reapplies required policy.
  catalog = [{ name: "optional", required: true, enabled: true }];
  await controller.refreshSkills();
  assert.deepEqual(controller.getSnapshot().skillSelection, {
    enable: [],
    disable: [],
  });
  controller.resetSkills();
  assert.equal(controller.getSnapshot().skills[0].enabled, true);
});

// Late catalogs cannot overwrite a newer refresh even when a transport ignores cancellation.
test("catalog refresh ignores stale responses and isolates errors", async () => {
  const old = deferred();
  let count = 0;
  const controller = new ChatController({
    agent: "a",
    transport: transport({
      listSkills: async () =>
        ++count === 1 ? old.promise : [{ name: "new", enabled: false }],
    }),
  });
  const pending = controller.refreshSkills();
  await controller.refreshSkills();
  old.resolve([{ name: "old", enabled: true }]);
  await pending;
  assert.equal(controller.getSnapshot().skills[0].name, "new");
  assert.equal(controller.getSnapshot().loadingSkills, false);

  // Missing optional transport support leaves chat usable with an empty catalog.
  const unsupported = new ChatController({
    agent: "a",
    transport: transport(),
  });
  await unsupported.refreshSkills();
  assert.deepEqual(unsupported.getSnapshot().skills, []);
});

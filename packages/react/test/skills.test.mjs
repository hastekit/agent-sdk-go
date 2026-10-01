import test from "node:test";
import assert from "node:assert/strict";
import { ChatController, createAGUITransport } from "../dist/index.js";
import { deferred, transport } from "./helpers.mjs";

// The catalog is the user's own library: not scoped to an agent, paged, and empty without a store.
test("skill catalog uses the agent-independent library endpoint", async () => {
  const signal = new AbortController().signal;
  const urls = [];
  const client = createAGUITransport({
    headers: { Authorization: "Bearer test" },
    fetch: async (url, init) => {
      urls.push(url);
      assert.equal(init.signal, signal);
      assert.equal(init.headers.get("Authorization"), "Bearer test");
      return url.endsWith("cursor=")
        ? Response.json({
            skills: [{ name: "review", description: "Review" }],
            nextCursor: "next",
          })
        : Response.json({ skills: [{ name: "notes", description: "Notes" }] });
    },
  });
  const skills = await client.listSkills(signal);
  assert.deepEqual(urls, [
    "/api/agui/skills?limit=200&cursor=",
    "/api/agui/skills?limit=200&cursor=next",
  ]);
  assert.deepEqual(
    skills.map((skill) => [skill.name, skill.enabled]),
    [
      ["review", true],
      ["notes", true],
    ],
  );

  const withoutStore = createAGUITransport({
    fetch: async () => new Response("404 page not found", { status: 404 }),
  });
  assert.deepEqual(await withoutStore.listSkills(signal), []);
});

// The user's choices must survive refresh and reach the outgoing run.
test("skill choices serialize per run", async () => {
  let input;
  let catalog = [
    { name: "mine", enabled: true },
    { name: "drafts", enabled: true },
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
  controller.setSkillEnabled("drafts", false);
  controller.setSkillEnabled("mine", false);
  controller.setSkillEnabled("mine", true);
  await controller.refreshSkills();
  await controller.sendMessage("hello");
  assert.deepEqual(input.forwardedProps, {
    tenant: "test",
    skills: { disable: ["drafts"] },
  });
  assert.deepEqual(
    controller.getSnapshot().skills.map((skill) => skill.enabled),
    [true, false],
  );

  // A skill deleted from the library drops its stale choice.
  catalog = [{ name: "mine", enabled: true }];
  await controller.refreshSkills();
  assert.deepEqual(controller.getSnapshot().skillSelection, { disable: [] });
  controller.setSkillEnabled("mine", false);
  controller.resetSkills();
  assert.equal(controller.getSnapshot().skills[0].enabled, true);
  assert.deepEqual(controller.getSnapshot().skillSelection, { disable: [] });
});

// Late catalogs cannot overwrite a newer refresh even when a transport ignores cancellation.
test("catalog refresh ignores stale responses and isolates errors", async () => {
  const old = deferred();
  let count = 0;
  const controller = new ChatController({
    agent: "a",
    transport: transport({
      listSkills: async () =>
        ++count === 1 ? old.promise : [{ name: "new", enabled: true }],
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

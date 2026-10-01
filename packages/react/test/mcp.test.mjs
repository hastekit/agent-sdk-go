import test from "node:test";
import assert from "node:assert/strict";
import { ChatController, createAGUITransport } from "../dist/index.js";
import { transport } from "./helpers.mjs";

// Management requests reuse chat credentials and encode server names.
test("MCP transport lists, saves, removes and builds OAuth URLs", async () => {
  const requests = [];
  const client = createAGUITransport({
    baseUrl: "https://app.example/api/agui",
    headers: { Authorization: "Bearer test" },
    fetch: async (url, init) => {
      requests.push([init.method ?? "GET", url, init.body ?? null]);
      assert.equal(init.headers.get("Authorization"), "Bearer test");
      if (url.endsWith("/mcp/"))
        return Response.json([
          { name: "docs", readOnly: true, oauth: false },
          {
            name: "mail",
            namespace: "alice",
            readOnly: false,
            oauth: true,
            connected: false,
          },
        ]);
      return new Response(null, { status: 204 });
    },
  });
  const listed = await client.mcp.list();
  assert.deepEqual(
    listed.map((server) => server.name),
    ["docs", "mail"],
  );
  await client.mcp.save("my mail", {
    endpoint: "https://mail.example/mcp",
    headers: { "X-Api-Key": "secret" },
  });
  await client.mcp.remove("my mail");
  assert.deepEqual(requests.slice(1), [
    [
      "PUT",
      "https://app.example/api/agui/mcp/my%20mail",
      JSON.stringify({
        endpoint: "https://mail.example/mcp",
        headers: { "X-Api-Key": "secret" },
      }),
    ],
    ["DELETE", "https://app.example/api/agui/mcp/my%20mail", null],
  ]);
  assert.equal(
    client.mcp.connectUrl("mail"),
    "https://app.example/api/agui/mcp/mail/connect",
  );
  assert.equal(
    client.mcp.callbackUrl("mail"),
    "https://app.example/api/agui/mcp/mail/callback",
  );

  // A server without an MCP store has no servers rather than an error.
  const withoutStore = createAGUITransport({
    fetch: async () => new Response("404 page not found", { status: 404 }),
  });
  assert.deepEqual(await withoutStore.mcp.list(), []);
});

// Globals stay on, user choices reach the run, and management refreshes the list.
test("MCP choices respect global servers and serialize per run", async () => {
  let input;
  let servers = [
    { name: "docs", namespace: "", readOnly: true, oauth: false },
    {
      name: "mail",
      namespace: "alice",
      readOnly: false,
      oauth: true,
      connected: false,
    },
    { name: "notes", namespace: "alice", readOnly: false, oauth: false },
  ];
  const saved = [];
  const controller = new ChatController({
    agent: "a",
    createId: () => "id",
    transport: transport({
      mcp: {
        list: async () => servers,
        save: async (name, config) => {
          saved.push([name, config]);
          servers = [
            ...servers,
            { name, namespace: "alice", readOnly: false, oauth: false },
          ];
        },
        remove: async (name) => {
          servers = servers.filter((server) => server.name !== name);
        },
        connectUrl: (name) => `/mcp/${name}/connect`,
        callbackUrl: (name) => `https://app.example/mcp/${name}/callback`,
      },
      stream: async function* (_agent, _thread, value) {
        input = value;
        yield { type: "RUN_FINISHED" };
      },
    }),
  });
  await controller.refreshMCPServers();
  assert.throws(() => controller.setMCPServerEnabled("docs", false), /global/);
  controller.setMCPServerEnabled("notes", false);
  controller.setMCPServerEnabled("mail", false);
  controller.setMCPServerEnabled("mail", true);
  await controller.sendMessage("hello");
  assert.deepEqual(input.forwardedProps, { mcp: { disable: ["notes"] } });
  assert.deepEqual(
    controller.getSnapshot().mcpServers.map((server) => server.enabled),
    [true, true, false],
  );
  assert.equal(controller.mcpConnectUrl("mail"), "/mcp/mail/connect");
  assert.equal(
    controller.mcpCallbackUrl("mail"),
    "https://app.example/mcp/mail/callback",
  );

  // Saving and removing reload the list; a removed server drops its stale choice.
  await controller.saveMCPServer("search", {
    endpoint: "https://search.example/mcp",
  });
  assert.deepEqual(saved, [
    ["search", { endpoint: "https://search.example/mcp" }],
  ]);
  assert.ok(
    controller
      .getSnapshot()
      .mcpServers.some((server) => server.name === "search"),
  );
  await controller.removeMCPServer("notes");
  assert.deepEqual(controller.getSnapshot().mcpSelection, { disable: [] });
  controller.setMCPServerEnabled("mail", false);
  controller.resetMCPServers();
  assert.ok(
    controller.getSnapshot().mcpServers.every((server) => server.enabled),
  );

  // Custom transports may omit MCP management entirely.
  const unsupported = new ChatController({
    agent: "a",
    transport: transport(),
  });
  await unsupported.refreshMCPServers();
  assert.deepEqual(unsupported.getSnapshot().mcpServers, []);
  await assert.rejects(
    unsupported.saveMCPServer("x", { endpoint: "https://x.example" }),
    /not supported/,
  );
});

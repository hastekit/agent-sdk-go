import test from "node:test";
import assert from "node:assert/strict";
import React from "react";
import TestRenderer, { act } from "react-test-renderer";
import { createAGUITransport, useRoutines } from "../dist/index.js";
import { deferred, transport } from "./helpers.mjs";

// Let React flush external-store updates deterministically in asynchronous tests.
globalThis.IS_REACT_ACT_ENVIRONMENT = true;
const definition = {
  name: "Daily",
  agent: "helper",
  instruction: "Summarize",
  schedule: { cron: "0 9 * * *", timezone: "UTC" },
};
const routine = { ...definition, id: "r/1", enabled: true };

// Every routine route must share authentication, encode IDs, and preserve write payloads.
test("routine transport covers CRUD, scheduler operations, and history", async () => {
  const requests = [];
  const signal = new AbortController().signal;
  const api = createAGUITransport({
    headers: () => ({ Authorization: "Bearer refreshed" }),
    credentials: "include",
    fetch: async (url, init) => {
      requests.push({ url, init });
      if (init.method === "DELETE") return new Response(null, { status: 204 });
      if (url.endsWith("/threads"))
        return Response.json({
          threads: [{ thread_id: "t", agent_name: "original" }],
        });
      if (url.endsWith("/agents")) return Response.json(["helper"]);
      if (url.endsWith("/routines") && init.method === "GET")
        return Response.json(null);
      return Response.json(routine);
    },
  }).routines;
  assert.deepEqual(await api.list(signal), []);
  assert.deepEqual(await api.agents(signal), ["helper"]);
  await api.get("r/1", signal);
  await api.create(definition, signal);
  await api.update("r/1", definition, signal);
  await api.setEnabled("r/1", false, signal);
  await api.setEnabled("r/1", true, signal);
  await api.runNow("r/1", signal);
  await api.status("r/1", signal);
  assert.equal((await api.threads("r/1", signal))[0].agent_name, "original");
  assert.equal(await api.delete("r/1", signal), undefined);

  // Assert the HTTP contract rather than implementation helper structure.
  assert.deepEqual(
    requests.map(({ url, init }) => [init.method, url]),
    [
      ["GET", "/api/agui/routines"],
      ["GET", "/api/agui/routines/agents"],
      ["GET", "/api/agui/routines/r%2F1"],
      ["POST", "/api/agui/routines"],
      ["PUT", "/api/agui/routines/r%2F1"],
      ["POST", "/api/agui/routines/r%2F1/pause"],
      ["POST", "/api/agui/routines/r%2F1/resume"],
      ["POST", "/api/agui/routines/r%2F1/run"],
      ["GET", "/api/agui/routines/r%2F1/status"],
      ["GET", "/api/agui/routines/r%2F1/threads"],
      ["DELETE", "/api/agui/routines/r%2F1"],
    ],
  );
  assert.deepEqual(JSON.parse(requests[3].init.body), definition);
  assert.deepEqual(JSON.parse(requests[4].init.body), definition);
  for (const { init } of requests) {
    assert.equal(init.signal, signal);
    assert.equal(init.headers.get("Authorization"), "Bearer refreshed");
    assert.equal(init.credentials, "include");
  }
});

// A failed scheduler request must surface once without accidentally creating duplicate runs.
test("routine mutations do not retry failures", async () => {
  let calls = 0;
  const api = createAGUITransport({
    fetch: async () => {
      calls++;
      return new Response("Scheduler unavailable", { status: 503 });
    },
  }).routines;
  await assert.rejects(api.runNow("r"), /Scheduler unavailable/);
  assert.equal(calls, 1);
});

// A delayed catalog must not overwrite a successful write and double clicks cannot submit twice.
test("routine hook serializes mutations and rejects stale refreshes", async () => {
  const stale = deferred();
  const write = deferred();
  let lists = 0,
    creates = 0,
    api,
    root;
  const client = transport({
    routines: {
      list: async () => (++lists === 1 ? [] : stale.promise),
      agents: async () => ["helper"],
      create: async () => {
        creates++;
        return write.promise;
      },
      setEnabled: async (_id, enabled) => ({ ...routine, enabled }),
      delete: async () => {},
    },
  });
  function App() {
    api = useRoutines({ transport: client });
    return null;
  }
  await act(async () => {
    root = TestRenderer.create(React.createElement(App));
  });
  assert.deepEqual(api.agents, ["helper"]);
  let refresh, saving;
  await act(async () => {
    refresh = api.refresh();
    saving = api.create(definition);
  });
  await act(async () => {
    await assert.rejects(api.create(definition), /already in progress/);
  });
  await act(async () => {
    write.resolve(routine);
    await saving;
    stale.resolve([]);
    await refresh;
  });
  assert.equal(creates, 1);
  assert.equal(api.routines[0].id, "r/1");
  await act(async () => {
    await api.setEnabled("r/1", false);
  });
  assert.equal(api.routines[0].enabled, false);
  await act(async () => {
    await api.delete("r/1");
  });
  assert.deepEqual(api.routines, []);
  await act(async () => root.unmount());
});

// Switching endpoint identity drops old lists, and unmount aborts outstanding reads.
test("routine hook ignores results from a previous transport", async () => {
  const old = deferred();
  let signal, api, root;
  const first = transport({
    routines: {
      list: async (value) => {
        signal = value;
        return old.promise;
      },
      agents: async () => [],
    },
  });
  const second = transport({
    routines: { list: async () => [routine], agents: async () => ["helper"] },
  });
  function App({ client }) {
    api = useRoutines({ transport: client });
    return null;
  }
  await act(async () => {
    root = TestRenderer.create(React.createElement(App, { client: first }));
  });
  await act(async () => {
    root.update(React.createElement(App, { client: second }));
  });
  assert.equal(signal.aborted, true);
  await act(async () => {
    old.resolve([]);
  });
  assert.equal(api.routines[0].id, "r/1");
  await act(async () => root.unmount());
});

// Routine occurrences must appear without manual refresh or switching the selected chat agent.
test("routine history follows lifecycle events and cancels its watcher", async () => {
  const { useRoutineThreads } = await import("../dist/index.js");
  const notification = deferred();
  let historyReads = 0,
    watches = 0,
    signal,
    api,
    root;
  const client = transport({
    routines: {
      threads: async () =>
        ++historyReads === 1
          ? []
          : [{ thread_id: "new-run", agent_name: "scheduled-agent" }],
    },
    watchRuns: async (agent, cursor, abort) => {
      assert.equal(agent, "scheduled-agent");
      signal = abort;
      watches++;
      if (!cursor) return notification.promise;
      return new Promise((resolve) =>
        abort.addEventListener(
          "abort",
          () => resolve({ supported: true, events: [], cursor }),
          { once: true },
        ),
      );
    },
  });
  function App() {
    api = useRoutineThreads({
      transport: client,
      routineId: "routine",
      agent: "scheduled-agent",
    });
    return null;
  }
  await act(async () => {
    root = TestRenderer.create(React.createElement(App));
  });
  assert.deepEqual(api.threads, []);

  // A new scheduled run announces its routine group and causes a fresh history read.
  await act(async () => {
    notification.resolve({
      supported: true,
      cursor: "1",
      events: [
        {
          event: "RUN_STARTED",
          threadId: "new-run",
          groupId: "routine",
          agentName: "scheduled-agent",
        },
      ],
    });
  });
  assert.equal(api.threads[0].thread_id, "new-run");
  assert.equal(historyReads, 2);
  assert.equal(watches, 1);
  await act(async () => root.unmount());
  assert.equal(signal.aborted, true);
});

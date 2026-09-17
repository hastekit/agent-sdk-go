import test from "node:test";
import assert from "node:assert/strict";
import { readFile, writeFile, mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import ts from "typescript";

const dir = await mkdtemp(join(tmpdir(), "hastekit-history-"));
for (const name of ["skill-upload", "api", "resumable-stream", "stoppable-agent"]) {
  const source = await readFile(new URL(`../src/${name}.ts`, import.meta.url), "utf8");
  let code = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ES2022, target: ts.ScriptTarget.ES2022 } }).outputText;
  for (const dep of ["yaml", "rxjs", "@ag-ui/core", "@ag-ui/client"]) code = code.replaceAll(`"${dep}"`, JSON.stringify(import.meta.resolve(dep)));
  for (const dep of ["skill-upload", "api", "resumable-stream"]) code = code.replaceAll(`"./${dep}"`, `"./${dep}.mjs"`);
  await writeFile(join(dir, `${name}.mjs`), code);
}
const { fetchMessages } = await import(pathToFileURL(join(dir, "api.mjs")));
const { StoppableHttpAgent } = await import(pathToFileURL(join(dir, "stoppable-agent.mjs")));
await rm(dir, { recursive: true, force: true });

test("history requests carry the page size and opaque cursor", async t => {
  t.mock.method(globalThis, "fetch", async url => {
    const parsed = new URL(url, "http://local");
    assert.equal(parsed.searchParams.get("limit"), "20");
    assert.equal(parsed.searchParams.get("cursor"), "opaque+cursor");
    return Response.json({ messages: [], run: null, nextCursor: "older" });
  });
  assert.equal((await fetchMessages("agent", "thread", "opaque+cursor", 20)).nextCursor, "older");
});

test("prepending history is ordered, deduplicated, and survives live events", async t => {
  globalThis.window = { location: { origin: "http://local" } };
  t.after(() => { delete globalThis.window; });
  const recent = { id: "recent", role: "user", content: "recent" };
  const older = { id: "older", role: "user", content: "older" };
  const agent = new StoppableHttpAgent({ agentName: "agent", url: "/run", threadId: "thread", history: [recent] });
  agent.subscribe({ onRunStartedEvent: () => {
    agent.prependHistory([older, recent]);
    agent.prependHistory([older]);
  }});
  t.mock.method(globalThis, "fetch", async (_url, init) => {
    const { runId } = JSON.parse(init.body);
    const events = [
      { type: "RUN_STARTED", threadId: "thread", runId },
      { type: "TEXT_MESSAGE_START", messageId: "reply", role: "assistant" },
      { type: "TEXT_MESSAGE_CONTENT", messageId: "reply", delta: "hello" },
      { type: "TEXT_MESSAGE_END", messageId: "reply" },
      { type: "RUN_FINISHED", threadId: "thread", runId },
    ];
    return new Response(events.map(e => `data: ${JSON.stringify(e)}\n\n`).join(""), { headers: { "Content-Type": "text/event-stream" } });
  });
  await agent.runAgent();
  assert.deepEqual(agent.messages.map(m => m.id), ["older", "recent", "reply"]);
});

test("skill selection accompanies new turns and approvals without replacing resume data", async t => {
  globalThis.window = { location: { origin: "http://local" } };
  t.after(() => { delete globalThis.window; });
  for (const fullHistory of [false, true]) {
    const agent = new StoppableHttpAgent({ agentName: "agent", url: "/run", threadId: "thread", fullHistory });
    agent.skillSelection = { enable: ["team/review"], disable: ["team/writing"] };
    const decisions = [{ toolCallId: "call", approved: true }];
    // requestInit is protected in TypeScript but available to this transport test.
    const request = agent.requestInit({ threadId: "thread", runId: "run", messages: [], tools: [], context: [], state: {}, forwardedProps: { command: { resume: { decisions } } } });
    const body = JSON.parse(request.body);
    assert.deepEqual(body.forwardedProps.skills, agent.skillSelection);
    assert.deepEqual(body.forwardedProps.command.resume.decisions, decisions);
  }
});

import type { BaseEvent } from "@ag-ui/core";
import { Subject, from, lastValueFrom, toArray, type Observable } from "rxjs";
import { describe, expect, it } from "vitest";
import { withClientTools, type ClientToolOptions, type ToolResult } from "../src/client-tools";

const call = [
  { type: "RUN_STARTED" },
  { type: "TOOL_CALL_START", toolCallId: "c1", toolCallName: "get_selection" },
  { type: "TOOL_CALL_ARGS", toolCallId: "c1", delta: '{"max":' },
  { type: "TOOL_CALL_ARGS", toolCallId: "c1", delta: "3}" },
  { type: "TOOL_CALL_END", toolCallId: "c1" },
] as BaseEvent[];

const success = { type: "RUN_FINISHED", outcome: { type: "success" } } as BaseEvent;
const events = (...list: object[]) => from(list as BaseEvent[]);
const collect = (stream: Observable<BaseEvent>) => lastValueFrom(stream.pipe(toArray())) as Promise<any[]>;
const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

// The run a sent result starts on the server: it resumes with the result.
const resumed = (results: ToolResult[]) =>
  events(
    { type: "RUN_STARTED" },
    { type: "TOOL_CALL_RESULT", toolCallId: "c1", content: results[0].content },
    { type: "RUN_FINISHED", outcome: { type: "success" } },
  );

// replies decides what each send comes to: "live" when a live run takes the
// results (204), "run" when they start a run.
function harness(replies: ("live" | "run")[] = []) {
  const ran: unknown[][] = [];
  const sent: ToolResult[][] = [];
  const options: ClientToolOptions = {
    runTool: (name, args, id) => {
      if (name !== "get_selection") return undefined;
      ran.push([name, args, id]);
      return Promise.resolve({ text: "selected" });
    },
    send: async (results) => {
      sent.push(results);
      return (replies[sent.length - 1] ?? "run") === "live" ? null : resumed(results);
    },
  };
  return { ran, sent, options };
}

describe("withClientTools", () => {
  it("sends the result to a live run, which reports it on the same stream", async () => {
    const t = harness(["live"]);
    const source = new Subject<BaseEvent>();
    const done = collect(withClientTools(source, t.options));
    call.forEach((e) => source.next(e));
    await tick();
    // The run took the result and carries on.
    source.next({ type: "TOOL_CALL_RESULT", toolCallId: "c1", content: "selected" } as BaseEvent);
    source.next(success);
    source.complete();

    const out = await done;
    expect(t.ran).toEqual([["get_selection", { max: 3 }, "c1"]]);
    expect(t.sent).toEqual([[{ toolCallId: "c1", content: '{"text":"selected"}' }]]);
    expect(out.at(-1).type).toBe("RUN_FINISHED");
  });

  it("resends a result a live run took but did not wait for, once the run pauses", async () => {
    const t = harness(["live", "run"]);
    const source = new Subject<BaseEvent>();
    const done = collect(withClientTools(source, t.options));
    call.forEach((e) => source.next(e));
    await tick();
    source.next(success);
    source.complete();

    const out = await done;
    expect(t.sent).toHaveLength(2);
    const types = out.map((e) => e.type);
    expect(types.filter((type) => type === "RUN_STARTED")).toHaveLength(1);
    expect(types.filter((type) => type === "RUN_FINISHED")).toHaveLength(1);
    expect(types).toContain("TOOL_CALL_RESULT");
  });

  it("continues with the run a result started when the run had already ended", async () => {
    const t = harness(["run"]);
    const source = new Subject<BaseEvent>();
    const done = collect(withClientTools(source, t.options));
    call.forEach((e) => source.next(e));
    await tick();
    source.next(success);
    source.complete();

    const out = await done;
    expect(t.sent).toHaveLength(1);
    expect(out.map((e) => e.type)).toContain("TOOL_CALL_RESULT");
  });

  it("sends once, after the run, a result that was not ready while it streamed", async () => {
    const t = harness();
    const out = await collect(withClientTools(events(...call, success), t.options));
    expect(t.sent).toEqual([[{ toolCallId: "c1", content: '{"text":"selected"}' }]]);
    expect(out.map((e) => e.type)).toContain("TOOL_CALL_RESULT");
  });

  it("honours an AG-UI 1.0 outcome that names the pending calls", async () => {
    const t = harness();
    await collect(withClientTools(events(...call, { type: "RUN_FINISHED", outcome: { type: "success", pendingToolCallIds: ["c1"] } }), t.options));
    expect(t.sent).toHaveLength(1);
  });

  it("leaves interrupts and other clients' tools alone", async () => {
    const t = harness();
    const interrupted = await collect(
      withClientTools(
        events(...call, { type: "RUN_FINISHED", outcome: { type: "interrupt", interrupts: [{ id: "c2", reason: "tool_call" }] } }),
        t.options,
      ),
    );
    expect(interrupted.at(-1).outcome.type).toBe("interrupt");
    expect(t.sent).toHaveLength(0);

    const other = harness();
    const foreign = call.map((e: any) => (e.toolCallName ? { ...e, toolCallName: "delete_user" } : e));
    await collect(withClientTools(events(...foreign, success), other.options));
    expect(other.ran).toHaveLength(0);
    expect(other.sent).toHaveLength(0);
  });

  it("sends every result when two tools finish after the run paused, as one run", async () => {
    const release: Record<string, () => void> = {};
    const sent: string[][] = [];
    const run = (ids: string[]) =>
      events(
        { type: "RUN_STARTED" },
        ...ids.map((id) => ({ type: "TOOL_CALL_RESULT", toolCallId: id, content: id })),
        { type: "RUN_FINISHED", outcome: { type: "success" } },
      );
    const options: ClientToolOptions = {
      runTool: (_name, _args, id) => new Promise((resolve) => (release[id] = () => resolve(`done ${id}`))),
      // Each result reaches a paused thread, so each starts the run that resumes it.
      send: async (results) => {
        sent.push(results.map((r) => r.toolCallId));
        return run(results.map((r) => r.toolCallId));
      },
    };
    const source = new Subject<BaseEvent>();
    const done = collect(withClientTools(source, options));
    for (const id of ["a1", "b1"]) {
      source.next({ type: "TOOL_CALL_START", toolCallId: id, toolCallName: "get_selection" } as BaseEvent);
      source.next({ type: "TOOL_CALL_END", toolCallId: id } as BaseEvent);
    }
    source.next(success);
    source.complete();
    release.a1();
    await tick();
    release.b1();

    const out = await done;
    expect(sent).toEqual([["a1"], ["b1"]]);
    expect(out.filter((e) => e.type === "TOOL_CALL_RESULT").map((e) => e.toolCallId)).toEqual(["a1", "b1"]);
    expect(out.filter((e) => e.type === "RUN_FINISHED")).toHaveLength(1);
  });

  it("in a joined run, answers only calls still unanswered when it ends", async () => {
    const t = harness();
    // A replay of a call already answered, then a live call left waiting.
    const replay = [
      { type: "RUN_STARTED" },
      { type: "TOOL_CALL_START", toolCallId: "old", toolCallName: "get_selection" },
      { type: "TOOL_CALL_END", toolCallId: "old" },
      { type: "TOOL_CALL_RESULT", toolCallId: "old", content: "earlier" },
      { type: "TOOL_CALL_START", toolCallId: "c1", toolCallName: "get_selection" },
      { type: "TOOL_CALL_ARGS", toolCallId: "c1", delta: '{"max":3}' },
      { type: "TOOL_CALL_END", toolCallId: "c1" },
      { type: "RUN_FINISHED", outcome: { type: "success" } },
    ];
    await collect(withClientTools(events(...replay), { ...t.options, eager: false }));
    expect(t.ran).toEqual([["get_selection", { max: 3 }, "c1"]]);
    expect(t.sent).toEqual([[{ toolCallId: "c1", content: '{"text":"selected"}' }]]);
  });

  it("answers with the error when a tool fails", async () => {
    const t = harness();
    t.options.runTool = () => Promise.reject(new Error("no selection"));
    await collect(withClientTools(events(...call, success), t.options));
    expect(t.sent).toEqual([[{ toolCallId: "c1", content: "Error: no selection" }]]);
  });
});

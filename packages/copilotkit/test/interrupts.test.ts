import type { Interrupt } from "@ag-ui/core";
import { describe, expect, it } from "vitest";
import { readInterrupt, toResumeEntries, toResumeEntry } from "../src/interrupts";

const approval: Interrupt = {
  id: "call_1",
  reason: "tool_call",
  toolCallId: "call_1",
  message: "Allow delete_user to run?",
  metadata: { mode: "approval", toolName: "delete_user", arguments: '{"id":"7"}' },
};
const form: Interrupt = {
  id: "call_2",
  reason: "input_required",
  toolCallId: "call_2",
  message: "Passenger details",
  responseSchema: { type: "object", properties: { name: { type: "string" } }, required: ["name"] },
  metadata: { mode: "form", toolName: "book_flight" },
};
const url: Interrupt = {
  id: "call_3",
  reason: "input_required",
  toolCallId: "call_3",
  metadata: { mode: "url", toolName: "link_account", url: "https://example.test/connect" },
};

describe("readInterrupt", () => {
  it("reads the server's mode and the paused call from metadata", () => {
    expect(readInterrupt(approval)).toMatchObject({
      id: "call_1",
      kind: "approval",
      toolCallId: "call_1",
      toolName: "delete_user",
      arguments: '{"id":"7"}',
      message: "Allow delete_user to run?",
    });
    expect(readInterrupt(form)).toMatchObject({ kind: "form", schema: form.responseSchema });
    expect(readInterrupt(url)).toMatchObject({ kind: "url", url: "https://example.test/connect" });
  });

  it("falls back to the reason when metadata has no mode", () => {
    expect(readInterrupt({ id: "a", reason: "tool_call" })).toMatchObject({ kind: "approval", toolCallId: "a" });
    expect(readInterrupt({ id: "b", reason: "input_required" }).kind).toBe("form");
  });
});

describe("toResumeEntry", () => {
  const [a, f, u] = [approval, form, url].map(readInterrupt);

  it("resolves approvals with the verdict, even a rejection", () => {
    expect(toResumeEntry(a, { id: a.id, approved: true })).toEqual({ interruptId: "call_1", status: "resolved", payload: { approved: true } });
    expect(toResumeEntry(a, { id: a.id, approved: false })).toEqual({ interruptId: "call_1", status: "resolved", payload: { approved: false } });
  });

  it("resolves forms with their values and visited URLs with nothing", () => {
    expect(toResumeEntry(f, { id: f.id, approved: true, values: { name: "Ada" } })).toEqual({
      interruptId: "call_2",
      status: "resolved",
      payload: { name: "Ada" },
    });
    expect(toResumeEntry(u, { id: u.id, approved: true })).toEqual({ interruptId: "call_3", status: "resolved" });
  });

  it("cancels declined elicitations", () => {
    expect(toResumeEntry(f, { id: f.id, approved: false, values: { name: "Ada" } })).toEqual({ interruptId: "call_2", status: "cancelled" });
    expect(toResumeEntry(u, { id: u.id, approved: false })).toEqual({ interruptId: "call_3", status: "cancelled" });
  });

  it("rejects decisions for interrupts that are not open", () => {
    expect(() => toResumeEntries([a], [{ id: "other", approved: true }])).toThrow(/other/);
  });
});

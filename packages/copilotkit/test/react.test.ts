import type { Interrupt } from "@ag-ui/core";
import { createElement } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

// The hooks are thin layers over CopilotKit's; these tests pin what they hand
// CopilotKit and what they do with what CopilotKit hands back.
const copilotkit = vi.hoisted(() => ({
  interruptConfig: undefined as any,
  getTool: vi.fn(),
  buildFrontendTools: vi.fn(),
  effects: [] as (() => void | (() => void))[],
}));

vi.mock("@copilotkit/react-core/v2", () => ({
  useInterrupt: (config: unknown) => {
    copilotkit.interruptConfig = config;
  },
  useCopilotKit: () => ({ copilotkit: { getTool: copilotkit.getTool, buildFrontendTools: copilotkit.buildFrontendTools } }),
}));

vi.mock("react", async (original) => ({
  ...(await original<typeof import("react")>()),
  useEffect: (effect: () => void | (() => void)) => {
    copilotkit.effects.push(effect);
  },
}));

const { useHastekitInterrupt, useHastekitClientTools } = await import("../src/react");
const { HastekitAgent } = await import("../src/agent");

beforeEach(() => {
  copilotkit.interruptConfig = undefined;
  copilotkit.effects = [];
  copilotkit.getTool.mockReset();
  copilotkit.buildFrontendTools.mockReset();
});

describe("useHastekitInterrupt", () => {
  const interrupts: Interrupt[] = [
    { id: "call_1", reason: "tool_call", metadata: { mode: "approval", toolName: "delete_user" } },
    { id: "call_2", reason: "input_required", metadata: { mode: "form" }, responseSchema: { type: "object" } },
  ];

  it("renders read interrupts and answers each through CopilotKit", () => {
    const rendered: any[] = [];
    useHastekitInterrupt({
      agentId: "support",
      renderInChat: false,
      render: (props) => {
        rendered.push(props);
        return createElement("div");
      },
    });
    expect(copilotkit.interruptConfig).toMatchObject({ agentId: "support", renderInChat: false });

    const resolve = vi.fn();
    const cancel = vi.fn();
    copilotkit.interruptConfig.render({ interrupts, resolve, cancel });
    const [{ interrupts: open, respond }] = rendered;
    expect(open.map((it: any) => [it.id, it.kind, it.toolName])).toEqual([
      ["call_1", "approval", "delete_user"],
      ["call_2", "form", undefined],
    ]);

    respond([
      { id: "call_1", approved: false },
      { id: "call_2", approved: false },
    ]);
    expect(resolve).toHaveBeenCalledWith({ approved: false }, "call_1");
    expect(cancel).toHaveBeenCalledWith("call_2");
  });
});

describe("useHastekitClientTools", () => {
  it("runs CopilotKit frontend tools for the agent and detaches on cleanup", async () => {
    const agent = new HastekitAgent({ agentName: "support", threadId: "thread" });
    const handler = vi.fn(async (args: { max: number }, _context?: unknown) => ({ text: "hi".repeat(args.max) }));
    copilotkit.getTool.mockImplementation(({ toolName }: { toolName: string }) =>
      toolName === "get_selection" ? { name: toolName, handler } : undefined,
    );

    useHastekitClientTools(agent);
    const cleanup = copilotkit.effects[0]();
    expect(await agent.clientToolRunner!("get_selection", { max: 2 }, "c1")).toEqual({ text: "hihi" });
    expect(copilotkit.getTool).toHaveBeenCalledWith({ toolName: "get_selection", agentId: "support" });
    expect(handler.mock.calls[0][1]).toMatchObject({ toolCall: { id: "c1" }, agent });
    expect(agent.clientToolRunner!("unknown", {}, "c2")).toBeUndefined();

    copilotkit.getTool.mockReturnValue({ name: "confirm", type: "human-in-the-loop", handler });
    expect(agent.clientToolRunner!("confirm", {}, "c3")).toBeUndefined();

    (cleanup as () => void)();
    expect(agent.clientToolRunner).toBeUndefined();
  });

  it("hands the agent CopilotKit's current tool list", () => {
    const agent = new HastekitAgent({ agentName: "support", threadId: "thread" });
    const tools = [{ name: "get_selection", description: "", parameters: { type: "object", properties: {} } }];
    copilotkit.buildFrontendTools.mockReturnValue(tools);

    useHastekitClientTools(agent);
    const cleanup = copilotkit.effects[0]();
    expect(agent.clientTools!()).toEqual(tools);
    expect(copilotkit.buildFrontendTools).toHaveBeenCalledWith("support");

    (cleanup as () => void)();
    expect(agent.clientTools).toBeUndefined();
  });
});

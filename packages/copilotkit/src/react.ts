"use client";

import type { Tool } from "@ag-ui/core";
import { useCopilotKit, useInterrupt } from "@copilotkit/react-core/v2";
import { useEffect, type ReactElement } from "react";
import type { HastekitAgent } from "./agent";
import { readInterrupt, toResumeEntries, type HastekitInterrupt, type InterruptDecision } from "./interrupts";

export interface HastekitInterruptRenderProps {
  /** The open interrupts, read into what a UI needs to render them. */
  interrupts: HastekitInterrupt[];
  /**
   * Answers interrupts. The run resumes once every open interrupt has an
   * answer, so answer them all, in one call or several.
   */
  respond: (decisions: InterruptDecision[]) => void;
}

export interface UseHastekitInterruptConfig<TRenderInChat extends boolean | undefined = undefined> {
  /** Renders the open interrupts. */
  render: (props: HastekitInterruptRenderProps) => ReactElement;
  /** The agent whose interrupts to handle. Defaults to the chat's agent. */
  agentId?: string;
  /**
   * When true (the default), the element renders inside `<CopilotChat>`. When
   * false, the hook returns it for you to place.
   */
  renderInChat?: TRenderInChat;
}

/**
 * Handles a HasteKit agent's interrupts: tool approvals, and MCP form and URL
 * elicitations. A thin layer over CopilotKit's `useInterrupt` that reads each
 * interrupt into a `HastekitInterrupt` and turns the user's decisions into the
 * AG-UI resume entries the server expects.
 *
 * ```tsx
 * useHastekitInterrupt({
 *   render: ({ interrupts, respond }) => (
 *     <ApprovalCard
 *       interrupts={interrupts}
 *       onSubmit={(approved) => respond(interrupts.map((it) => ({ id: it.id, approved })))}
 *     />
 *   ),
 * });
 * ```
 *
 * A page that loads while the agent is already waiting never sees the run that
 * raised the interrupt. Read the waiting interrupts from `fetchThreadMessages`
 * and answer them with `agent.resume(toResumeEntries(...))`.
 */
export function useHastekitInterrupt<TRenderInChat extends boolean | undefined = undefined>(
  config: UseHastekitInterruptConfig<TRenderInChat>,
) {
  const { render, agentId, renderInChat } = config;
  return useInterrupt<never, TRenderInChat>({
    agentId,
    renderInChat,
    render: ({ interrupts, resolve, cancel }) => {
      const open = interrupts.map(readInterrupt);
      const respond = (decisions: InterruptDecision[]) => {
        // useInterrupt resumes once every open interrupt is addressed.
        for (const entry of toResumeEntries(open, decisions)) {
          if (entry.status === "cancelled") void cancel(entry.interruptId);
          else void resolve(entry.payload, entry.interruptId);
        }
      };
      return render({ interrupts: open, respond });
    },
  });
}

export interface UseHastekitClientToolsOptions {
  /**
   * Client tool calls the thread is paused on, as a thread's run state reports
   * them (`fetchThreadMessages(...).run.pendingToolCallIds`). They are answered
   * once the tools are bound, so a thread opened after a reload does not stay
   * paused waiting for a client that already went away.
   */
  pendingToolCallIds?: string[];
}

// The tool list CopilotKit sends with a run. Core builds it with a method its
// types keep for its own classes (`CopilotKitCoreFriendsAccess`); it is looked
// up rather than assumed, so a CopilotKit without it falls back to the tools
// the agent last saw.
interface FrontendToolBuilder {
  buildFrontendTools?: (agentId?: string) => Tool[];
}

/**
 * Lets a HasteKit agent run the tools registered with CopilotKit
 * (`useFrontendTool`) as soon as their calls stream in, instead of after the
 * run ends. This is what lets a server that waits for client tool results get
 * them within the same run. Human-in-the-loop tools wait on a person, so
 * CopilotKit keeps running those itself.
 *
 * It also hands the agent CopilotKit's current tool list, which the agent
 * sends with the requests it makes itself — client tool results, steering,
 * resumes — so the run such a request starts can still call the client's
 * tools, even on a page that has not run anything since it loaded.
 *
 * Call it once, inside the `CopilotKitProvider` the agent is registered with.
 */
export function useHastekitClientTools(
  agent: HastekitAgent | null | undefined,
  options: UseHastekitClientToolsOptions = {},
): void {
  const { copilotkit } = useCopilotKit();
  const pending = (options.pendingToolCallIds ?? []).join(",");
  useEffect(() => {
    if (!agent) return;
    agent.clientToolRunner = (name, args, toolCallId) => {
      const tool = copilotkit.getTool({ toolName: name, agentId: agent.agentId });
      if (!tool?.handler || tool.type === "human-in-the-loop") return undefined;
      return Promise.resolve(
        tool.handler(args as Record<string, unknown>, {
          toolCall: { id: toolCallId, type: "function", function: { name, arguments: JSON.stringify(args) } },
          agent,
        }),
      );
    };
    const builder = copilotkit as unknown as FrontendToolBuilder;
    if (typeof builder.buildFrontendTools === "function") {
      agent.clientTools = () => builder.buildFrontendTools!(agent.agentId);
    }
    if (pending) void agent.answerClientTools(pending.split(",")).catch(() => {});
    return () => {
      agent.clientToolRunner = undefined;
      agent.clientTools = undefined;
    };
  }, [agent, copilotkit, pending]);
}

export type { HastekitInterrupt, InterruptDecision } from "./interrupts";

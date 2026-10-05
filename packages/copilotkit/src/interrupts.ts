import type { Interrupt, ResumeEntry } from "@ag-ui/core";

/**
 * What an interrupt asks of the user:
 *
 * - `approval`: allow or reject a tool call.
 * - `form`: fill in fields described by a JSON schema (an MCP form elicitation).
 * - `url`: visit a link, then continue (an MCP URL elicitation).
 */
export type InterruptKind = "approval" | "form" | "url";

/** One field of a form interrupt, as its JSON schema describes it. */
export interface FormField {
  type?: string;
  title?: string;
  description?: string;
  enum?: string[];
  default?: unknown;
}

/** The JSON schema of a form interrupt. */
export interface FormSchema {
  type?: string;
  properties?: Record<string, FormField>;
  required?: string[];
}

/** An AG-UI interrupt from a HasteKit server, read into what a UI needs to render it. */
export interface HastekitInterrupt {
  /** The interrupt id, which is also the paused tool call's id. */
  id: string;
  kind: InterruptKind;
  toolCallId: string;
  /** The paused tool's name. */
  toolName?: string;
  /** The paused call's arguments, as JSON text. */
  arguments?: string;
  message?: string;
  /** For a form: the fields to ask for. */
  schema?: FormSchema;
  /** For a URL elicitation: where to send the user. */
  url?: string;
  /** The interrupt as the server sent it. */
  raw: Interrupt;
}

/**
 * The user's answer to one interrupt. `approved: false` rejects an approval and
 * cancels a form or URL elicitation; a submitted form carries its `values`.
 */
export interface InterruptDecision {
  id: string;
  approved: boolean;
  values?: Record<string, unknown>;
}

interface HastekitInterruptMetadata {
  mode?: string;
  toolName?: string;
  arguments?: string;
  url?: string;
}

/** Reads an AG-UI interrupt from a HasteKit server. */
export function readInterrupt(interrupt: Interrupt): HastekitInterrupt {
  const metadata = (interrupt.metadata ?? {}) as HastekitInterruptMetadata;
  const kind = interruptKind(interrupt, metadata);
  return {
    id: interrupt.id,
    kind,
    toolCallId: interrupt.toolCallId ?? interrupt.id,
    toolName: metadata.toolName,
    arguments: metadata.arguments,
    message: interrupt.message,
    schema: kind === "form" ? (interrupt.responseSchema as FormSchema | undefined) : undefined,
    url: metadata.url,
    raw: interrupt,
  };
}

function interruptKind(interrupt: Interrupt, metadata: HastekitInterruptMetadata): InterruptKind {
  if (metadata.mode === "approval" || metadata.mode === "form" || metadata.mode === "url") return metadata.mode;
  return interrupt.reason === "tool_call" ? "approval" : "form";
}

/**
 * Builds the AG-UI resume entry that answers an interrupt: an approval resolves
 * with `{ approved }`, a form with its values, a visited URL with nothing, and a
 * declined form or URL is cancelled.
 */
export function toResumeEntry(interrupt: HastekitInterrupt, decision: InterruptDecision): ResumeEntry {
  const interruptId = interrupt.id;
  if (interrupt.kind === "approval") return { interruptId, status: "resolved", payload: { approved: decision.approved } };
  if (!decision.approved) return { interruptId, status: "cancelled" };
  if (interrupt.kind === "form") return { interruptId, status: "resolved", payload: decision.values ?? {} };
  return { interruptId, status: "resolved" };
}

/** Builds the resume entries for a set of decisions. Every decision must name one of the interrupts. */
export function toResumeEntries(interrupts: HastekitInterrupt[], decisions: InterruptDecision[]): ResumeEntry[] {
  return decisions.map((decision) => {
    const interrupt = interrupts.find((it) => it.id === decision.id);
    if (!interrupt) throw new Error(`No open interrupt with id ${decision.id}`);
    return toResumeEntry(interrupt, decision);
  });
}

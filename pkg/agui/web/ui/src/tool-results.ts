import { createContext } from "react";

// Finds a tool call's stored result message by call id, so its card can show
// when the result came back (metadata.createdAt, as for other messages).
export interface ToolResultMessage {
  id: string;
  role: string;
  toolCallId?: string;
  metadata?: Record<string, unknown>;
}

export const ToolResultLookup = createContext<(toolCallId: string) => ToolResultMessage | undefined>(() => undefined);

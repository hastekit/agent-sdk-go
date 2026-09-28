import jsonPatch from "fast-json-patch";
import type { Operation } from "fast-json-patch";
import type {
  ChatEvent,
  ChatSnapshot,
  Interrupt,
  Message,
  ToolCall,
} from "./types.js";

// Merge history by identity while keeping the live version of overlapping messages.
export function prependMessages(
  older: Message[],
  current: Message[],
): Message[] {
  const currentIds = new Set(current.map((message) => message.id));
  const callIds = new Set(
    current.flatMap(
      (message) => message.toolCalls?.map((call) => call.id) ?? [],
    ),
  );
  const resultIds = new Set(
    current
      .filter((message) => message.role === "tool")
      .map((message) => message.toolCallId)
      .filter(Boolean),
  );

  // History and streaming can assign different message IDs to the same tool call.
  const retained = older.flatMap((message) => {
    if (currentIds.has(message.id)) return [];
    if (
      message.role === "tool" &&
      message.toolCallId &&
      resultIds.has(message.toolCallId)
    )
      return [];
    if (!message.toolCalls?.length) return [message];
    const toolCalls = message.toolCalls.filter((call) => !callIds.has(call.id));
    if (!toolCalls.length && !message.content) return [];
    return [{ ...message, toolCalls }];
  });
  return [...retained, ...current];
}

// Replace an existing message in place or append a new message to the transcript.
function upsert(messages: Message[], message: Message): Message[] {
  const index = messages.findIndex((item) => item.id === message.id);
  if (index < 0) return [...messages, message];
  return messages.map((item, position) =>
    position === index ? message : item,
  );
}

// Read protocol identifiers without coercing missing values into visible text.
function text(event: ChatEvent, key: string): string {
  return typeof event[key] === "string" ? (event[key] as string) : "";
}

// Keep replay-local bookkeeping separate from the serializable React snapshot.
export class EventReducer {
  private readonly echoedUsers = new Set<string>();
  private readonly completedCalls = new Set<string>();
  // Tool calls this run started that have no result yet.
  private readonly unanswered = new Set<string>();
  private chunkMessageId = "";
  private chunkToolId = "";

  // Apply standard AG-UI events and the SDK's chat-related custom events.
  apply(snapshot: ChatSnapshot, event: ChatEvent): Partial<ChatSnapshot> {
    const messages = snapshot.messages;
    const id = text(event, "messageId");

    // Compact text events carry optional start metadata followed by a delta.
    if (event.type === "TEXT_MESSAGE_CHUNK") {
      const messageId = id || this.chunkMessageId;
      if (!messageId) throw new Error("Text chunk has no message identity");
      let next = snapshot;
      if (messageId !== this.chunkMessageId) {
        this.chunkMessageId = messageId;
        next = {
          ...next,
          ...this.apply(next, {
            ...event,
            type: "TEXT_MESSAGE_START",
            messageId,
          }),
        };
      }
      return {
        ...this.apply(next, {
          ...event,
          type: "TEXT_MESSAGE_CONTENT",
          messageId,
        }),
      };
    }

    // Compact tool events initialize once and then append argument fragments.
    if (event.type === "TOOL_CALL_CHUNK") {
      const toolCallId = text(event, "toolCallId") || this.chunkToolId;
      if (!toolCallId) throw new Error("Tool chunk has no call identity");
      let next = snapshot;
      if (toolCallId !== this.chunkToolId) {
        this.chunkToolId = toolCallId;
        next = {
          ...next,
          ...this.apply(next, {
            ...event,
            type: "TOOL_CALL_START",
            toolCallId,
          }),
        };
      }
      return this.apply(next, { ...event, type: "TOOL_CALL_ARGS", toolCallId });
    }

    // Text starts reset replayed assistant content, while existing user input is an echo.
    if (
      event.type === "TEXT_MESSAGE_START" ||
      event.type === "REASONING_MESSAGE_START"
    ) {
      const role =
        event.type === "REASONING_MESSAGE_START"
          ? "reasoning"
          : ((text(event, "role") || "assistant") as Message["role"]);
      const existing = messages.find((message) => message.id === id);
      if (existing && role === "user") {
        this.echoedUsers.add(id);
        return {};
      }
      return {
        messages: upsert(messages, { ...existing, id, role, content: "" }),
      };
    }

    // Append deltas only after identifying their target message.
    if (
      event.type === "TEXT_MESSAGE_CONTENT" ||
      event.type === "REASONING_MESSAGE_CONTENT"
    ) {
      if (this.echoedUsers.has(id)) return {};
      const existing = messages.find((message) => message.id === id);
      const role =
        event.type === "REASONING_MESSAGE_CONTENT" ? "reasoning" : "assistant";
      return {
        messages: upsert(messages, {
          ...existing,
          id,
          role: existing?.role ?? role,
          content:
            (typeof existing?.content === "string" ? existing.content : "") +
            text(event, "delta"),
        }),
      };
    }

    // End markers release echo suppression without removing completed messages.
    if (
      event.type === "TEXT_MESSAGE_END" ||
      event.type === "REASONING_MESSAGE_END"
    ) {
      this.echoedUsers.delete(id);
      return {};
    }

    // Tool calls belong to an assistant message and collect JSON arguments as text.
    if (event.type === "TOOL_CALL_START") {
      const callId = text(event, "toolCallId");
      // Replay must reuse a persisted carrier rather than append a synthetic one.
      const owner = messages.find((message) =>
        message.toolCalls?.some((call) => call.id === callId),
      );
      // Completed history is authoritative when replay repeats an already executed call.
      if (
        owner &&
        messages.some(
          (message) => message.role === "tool" && message.toolCallId === callId,
        )
      ) {
        this.completedCalls.add(callId);
        this.unanswered.delete(callId);
        return {};
      }
      this.unanswered.add(callId);
      const parent =
        owner?.id || text(event, "parentMessageId") || `tool-parent-${callId}`;

      const existing = messages.find((message) => message.id === parent);
      const call: ToolCall = {
        id: callId,
        type: "function",
        function: { name: text(event, "toolCallName"), arguments: "" },
      };
      const calls = existing?.toolCalls ?? [];
      return {
        messages: upsert(messages, {
          ...existing,
          id: parent,
          role: "assistant",
          toolCalls: [...calls.filter((item) => item.id !== callId), call],
        }),
      };
    }

    // Argument updates preserve other calls and content on the same assistant message.
    if (event.type === "TOOL_CALL_ARGS") {
      const callId = text(event, "toolCallId");
      if (this.completedCalls.has(callId)) return {};
      return {
        messages: messages.map((message) => ({
          ...message,
          toolCalls: message.toolCalls?.map((call) =>
            call.id !== callId
              ? call
              : {
                  ...call,
                  function: {
                    ...call.function,
                    arguments: call.function.arguments + text(event, "delta"),
                  },
                },
          ),
        })),
      };
    }

    // Tool results use their own message identity to remain stable across replay.
    if (event.type === "TOOL_CALL_RESULT") {
      const callId = text(event, "toolCallId");
      this.unanswered.delete(callId);
      const existing = messages.find(
        (message) => message.role === "tool" && message.toolCallId === callId,
      );
      return {
        messages: upsert(messages, {
          id: existing?.id || id || `tool-result-${callId}`,
          role: "tool",
          toolCallId: callId,
          content: text(event, "content"),
        }),
      };
    }

    // A run snapshot updates known messages without dropping older paginated history.
    if (event.type === "MESSAGES_SNAPSHOT") {
      const incoming = event.messages as Message[];
      return { messages: prependMessages(messages, incoming) };
    }

    // Agent state is independent of the transcript and supports RFC 6902 updates.
    if (event.type === "STATE_SNAPSHOT")
      return { state: event.snapshot as Record<string, unknown> };
    if (event.type === "STATE_DELTA") {
      return {
        state: jsonPatch.applyPatch(
          snapshot.state,
          event.delta as Operation[],
          true,
          false,
        ).newDocument,
      };
    }

    // Run lifecycle events retain approval data supplied by custom events.
    if (event.type === "RUN_STARTED") {
      this.unanswered.clear();
      return {
        error: null,
        run: {
          runId: text(event, "runId"),
          status: "running",
          awaitingApproval: false,
        },
      };
    }
    // AG-UI 1.0: the outcome says whether the run is waiting on a person
    // (interrupt). A successful run whose tool calls are not all answered left
    // them for this client; like @ag-ui/client, prefer the outcome's
    // pendingToolCallIds and otherwise take the calls that got no result.
    if (event.type === "RUN_FINISHED") {
      const outcome = event.outcome as
        | {
            type?: string;
            interrupts?: Interrupt[];
            pendingToolCallIds?: string[];
          }
        | undefined;
      const runId = text(event, "runId") || snapshot.run?.runId;
      if (outcome?.type === "interrupt" && outcome.interrupts?.length)
        return {
          run: {
            ...snapshot.run,
            runId,
            status: "paused",
            awaitingApproval: outcome.interrupts.some(
              (it) => it.reason === "tool_call",
            ),
            interrupts: outcome.interrupts,
          },
          isCompacting: false,
        };
      const pending = outcome?.pendingToolCallIds?.length
        ? outcome.pendingToolCallIds
        : [...this.unanswered];
      this.unanswered.clear();
      if (outcome?.type !== "interrupt" && pending.length)
        return {
          run: {
            ...snapshot.run,
            runId,
            status: "paused",
            awaitingApproval: false,
            pendingToolCallIds: pending,
          },
          isCompacting: false,
        };
      return {
        run: snapshot.run?.backgroundTasks?.length
          ? {
              ...snapshot.run,
              status: "completed",
              awaitingApproval: false,
              interrupts: undefined,
              pendingToolCallIds: undefined,
            }
          : null,
        isCompacting: false,
      };
    }
    if (event.type === "RUN_ERROR")
      return {
        error: new Error(text(event, "message") || "Agent run failed"),
        run: null,
        isCompacting: false,
      };

    // Custom events expose persisted input, approvals, and context compaction to any renderer.
    if (event.type === "CUSTOM") {
      if (event.name === "input_message")
        return { messages: upsert(messages, event.value as Message) };
      if (event.name === "hastekit.summarization_started")
        return { isCompacting: true };
      if (event.name === "hastekit.summarization_completed")
        return { isCompacting: false };
    }

    // Unknown events remain available through lastEvent and the application callback.
    return {};
  }
}

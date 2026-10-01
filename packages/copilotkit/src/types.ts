import type { Interrupt, Message } from "@ag-ui/core";

/** How to reach a HasteKit agent: its name on the server and the AG-UI handler it is mounted on. */
export interface HastekitConnection {
  /** The name the agent is registered under on the HasteKit server. */
  agentName: string;
  /** Base URL of the HasteKit AG-UI handler. Defaults to `/api/agui`. */
  baseUrl?: string;
  /** Headers sent with every request, such as authorization. */
  headers?: Record<string, string>;
  /** A fetch implementation to use instead of the global one. */
  fetch?: (url: string, init: RequestInit) => Promise<Response>;
}

/** Turns off some of the user's own skills for a run. An agent's global skills are always on. */
export interface SkillSelection {
  disable: string[];
}

/** Turns off some of the user's own MCP servers for a run. Global servers are always on. */
export interface MCPSelection {
  disable: string[];
}

/** A tool that kept working after its call was answered. */
export interface BackgroundTask {
  taskId: string;
  callId?: string;
  toolName?: string;
  streamId?: string;
  startedAt?: string;
}

/** What a thread's last run left outstanding. */
export interface ThreadRunState {
  runId?: string;
  status?: string;
  error?: string;
  /** Whether any open interrupt is a tool approval. */
  awaitingApproval: boolean;
  /** Interrupts a person must answer, as the run's interrupt outcome carried them. */
  interrupts?: Interrupt[];
  /** Client tool calls left for the client to answer with tool messages. */
  pendingToolCallIds?: string[];
  backgroundTasks?: BackgroundTask[];
}

/**
 * A thread's current context size: the tokens the next prompt starts from.
 */
export interface ContextUsage {
  tokens: number;
  agentName?: string;
}

/** One page of a thread's history, oldest first. */
export interface ThreadPage {
  messages: Message[];
  /** What the thread's last run left outstanding, or null when it is settled. */
  run: ThreadRunState | null;
  /**
   * The context token count when the last run ended, or null for a
   * thread with nothing in it yet. Runs report it live as `ContextUsage` events.
   */
  context: ContextUsage | null;
  /** Cursor for the next older page; empty when there is none. */
  nextCursor: string;
  /** The attachment session for this thread. */
  sessionId: string;
}

/** A run starting or finishing anywhere the server lets this user see. */
export interface RunFeedEvent {
  event: "RUN_STARTED" | "RUN_FINISHED";
  namespace?: string;
  groupId?: string;
  threadId: string;
  runId?: string;
  agentName?: string;
  streamId: string;
  at?: string;
}

/** CUSTOM event names a HasteKit server emits. */
export const HastekitEvent = {
  /** A turn the run took in, including turns folded into it mid-run. */
  InputMessage: "input_message",
  /** The run's stream id, first thing on every run. */
  StreamId: "hastekit.stream_id",
  ToolProgress: "hastekit.tool_progress",
  FileGenerated: "hastekit.file_generated",
  Annotation: "hastekit.annotation",
  BackgroundTaskStarted: "hastekit.background_task_started",
  BackgroundTaskCompleted: "hastekit.background_task_completed",
  SummarizationStarted: "hastekit.summarization_started",
  SummarizationCompleted: "hastekit.summarization_completed",
  /** The context token count after each model call. The value is a `ContextUsage`. */
  ContextUsage: "hastekit.context_usage",
} as const;

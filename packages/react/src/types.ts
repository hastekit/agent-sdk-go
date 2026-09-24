import type { RoutineTransport } from "./routines.js";

// Describe agent-visible skills and their host-controlled enablement policy.
export interface AgentSkill {
  name: string;
  description: string;
  file_location: string;
  resources?: string[];
  required?: boolean;
  defaultEnabled?: boolean;
  global?: boolean;
  enabled: boolean;
}

// Override agent defaults by plain skill name for an outgoing run.
export interface SkillSelection {
  enable: string[];
  disable: string[];
}

// Preserve multipart AG-UI content without coupling consumers to a renderer.
export interface ContentPart {
  type: string;
  [key: string]: unknown;
}

// Expose function calls using the standard AG-UI message shape.
export interface ToolCall {
  id: string;
  type: "function";
  function: { name: string; arguments: string };
}

/** AG-UI wire message. Multipart input is preserved for application renderers. */
export interface Message {
  id: string;
  role: "user" | "assistant" | "system" | "developer" | "tool" | "reasoning";
  content?: string | ContentPart[];
  toolCalls?: ToolCall[];
  toolCallId?: string;
  name?: string;
  encryptedValue?: string;
}

// Keep server-provided sidebar metadata and identities intact.
export interface Thread {
  thread_id: string;
  title: string;
  conversation_id?: string;
  group_id?: string;
  agent_name?: string;
  namespace?: string;
  message_count?: number;
  created_at?: string;
  updated_at?: string;
}

// Restore outstanding approvals and background task metadata with history.
export interface RunState {
  error?: string;
  runId?: string;
  status?: string;
  awaitingApproval: boolean;
  interrupts?: Record<string, unknown>[];
  pendingToolCalls?: Record<string, unknown>[];
  backgroundTasks?: Record<string, unknown>[];
}

// Represent one server history page and its opaque continuation cursor.
export interface MessagePage {
  messages: Message[];
  run: RunState | null;
  nextCursor: string;
  sessionId: string;
}

// Allow standard and application-defined events through the same callback.
export interface ChatEvent {
  type: string;
  [key: string]: unknown;
}

// Describe the complete request envelope accepted by the SDK AG-UI handler.
export interface RunInput {
  threadId: string;
  runId: string;
  messages: Message[];
  state: Record<string, unknown>;
  tools: unknown[];
  context: { description: string; value: string }[];
  forwardedProps: Record<string, unknown>;
}

// Track lifecycle changes originating outside the selected conversation.
export interface RunFeedEvent {
  event: "RUN_STARTED" | "RUN_FINISHED";
  threadId: string;
  groupId?: string;
  runId?: string;
  streamId: string;
  agentName?: string;
}

// Return file metadata that applications can use in multipart messages.
export interface Attachment {
  file_id: string;
  url: string;
  filename: string;
  mediaType: string;
  size: number;
}

// Distinguish connection recovery from observed agent execution.
export type ConnectionStatus =
  "idle" | "connecting" | "streaming" | "reconnecting";
// Scope event delivery and connection status to one subscription.
export interface StreamOptions {
  // A run feed can announce ownership before its first stream event is published.
  waitForRun?: boolean;
  signal: AbortSignal;
  onStatus?: (status: ConnectionStatus) => void;
}

/** Implement this interface to use a different backend without changing your UI. */
export interface ChatTransport {
  routines?: RoutineTransport;
  listSkills?(agent: string, signal: AbortSignal): Promise<AgentSkill[]>;
  listThreads(
    agent: string,
    group: string,
    signal: AbortSignal,
  ): Promise<{ threads: Thread[]; supported: boolean }>;
  loadMessages(
    agent: string,
    thread: string,
    cursor: string,
    signal: AbortSignal,
  ): Promise<MessagePage>;
  stream(
    agent: string,
    thread: string,
    input: RunInput | undefined,
    options: StreamOptions,
  ): AsyncIterable<ChatEvent>;
  stop(
    agent: string,
    thread: string,
    streamId: string | undefined,
    signal: AbortSignal,
  ): Promise<void>;
  watchRuns?(
    agent: string,
    cursor: string,
    signal: AbortSignal,
  ): Promise<{ events: RunFeedEvent[]; cursor: string; supported: boolean }>;
  uploadAttachment?(
    file: File,
    sessionId: string,
    signal?: AbortSignal,
  ): Promise<Attachment>;
}

// Expose rendering state without leaking controllers or network handles.
export interface ChatSnapshot {
  skills: AgentSkill[];
  skillSelection: SkillSelection;
  loadingSkills: boolean;
  skillsError: Error | null;
  threads: Thread[];
  threadsSupported: boolean;
  loadingThreads: boolean;
  threadId: string | null;
  sessionId: string | null;
  messages: Message[];
  loadingMessages: boolean;
  loadingOlder: boolean;
  hasOlderMessages: boolean;
  connection: ConnectionStatus;
  isRunning: boolean;
  isStopping: boolean;
  isCompacting: boolean;
  run: RunState | null;
  state: Record<string, unknown>;
  activeThreadIds: string[];
  error: Error | null;
  lastEvent: ChatEvent | null;
}

// Configure one controller for an agent and its conversation group.
export interface ChatOptions {
  agent: string;
  transport: ChatTransport;
  groupId?: string;
  initialThreadId?: string;
  fullHistory?: boolean;
  watchRuns?: boolean;
  forwardedProps?: Record<string, unknown>;
  context?: RunInput["context"];
  onEvent?: (event: ChatEvent) => void;
  createId?: () => string;
}

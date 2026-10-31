import type { RoutineTransport } from "./routines.js";

// Describe one of the user's own skills. These are shared by every agent, so the
// catalog is not scoped to one; each is on unless disabled. An agent's global
// skills are configured on the server, always on, and never listed here.
export interface UserSkill {
  name: string;
  description: string;
  resources?: string[];
  enabled: boolean;
}

// Turn off the user's own skills by plain name for an outgoing run.
export interface SkillSelection {
  disable: string[];
}

// One MCP server the user can see. Global servers are the developer's: always on
// and read-only. The user's own servers are on unless disabled.
export interface MCPServer {
  name: string;
  // Empty for a global server; the owner's namespace for the user's own.
  namespace?: string;
  transport?: string;
  readOnly: boolean;
  // OAuth servers need the user to connect an account before their tools load.
  oauth: boolean;
  // Present when the server offers OAuth connect: whether this user has connected.
  connected?: boolean;
  // Why one of the user's definitions is unusable; delete or replace it to recover.
  error?: string;
  enabled: boolean;
}

// Turn off the user's own MCP servers by name for an outgoing run.
export interface MCPSelection {
  disable: string[];
}

// OAuth settings for a user-owned MCP server. Only redirectUrl is required:
// without a client ID the server discovers the MCP server's authorization server
// and registers a client there. authUrl and tokenUrl pin the endpoints for
// servers that publish no metadata, and need a clientId.
export interface MCPOAuthConfig {
  clientId?: string;
  clientSecret?: string;
  authUrl?: string;
  tokenUrl?: string;
  // Must be this server's callback route; see MCPTransport.callbackUrl.
  redirectUrl: string;
  scopes?: string[];
}

// A user-owned remote MCP server. Headers carry literal values such as API keys;
// the server rejects templates and private network addresses.
export interface MCPServerConfig {
  endpoint: string;
  transport?: "streamable-http" | "sse";
  toolPrefix?: string;
  headers?: Record<string, string>;
  authorization?: MCPOAuthConfig;
}

// Manage the user's MCP servers. Optional so custom backends can omit it.
export interface MCPTransport {
  list(signal?: AbortSignal): Promise<Omit<MCPServer, "enabled">[]>;
  save(
    name: string,
    config: MCPServerConfig,
    signal?: AbortSignal,
  ): Promise<void>;
  remove(name: string, signal?: AbortSignal): Promise<void>;
  // Browser URL that starts OAuth for a server; open it in a new tab or window.
  connectUrl(name: string): string;
  // Absolute callback URL to use as a server's OAuth redirect URL.
  callbackUrl(name: string): string;
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
  // Empty for user-started threads; the spawning thread for agent-started ones.
  parent_thread_id?: string;
  // Internal threads (sub-agent conversations); omitted from listings by default.
  hidden?: boolean;
  agent_name?: string;
  namespace?: string;
  message_count?: number;
  created_at?: string;
  updated_at?: string;
}

// An AG-UI 1.0 interrupt: something the run is waiting on a person for. The id
// is the paused tool call's id; metadata carries the server's mode ("approval",
// "form" or "url") and the call, for rendering.
export interface Interrupt {
  id: string;
  reason: string;
  message?: string;
  toolCallId?: string;
  responseSchema?: Record<string, unknown>;
  expiresAt?: string;
  metadata?: Record<string, unknown>;
}

// Answer one interrupt. Approvals resolve with { approved: boolean }, forms
// with their fields, visited URLs with no payload; "cancelled" declines.
export interface ResumeEntry {
  interruptId: string;
  status: "resolved" | "cancelled";
  payload?: unknown;
}

// Restore outstanding interrupts and background task metadata with history.
export interface RunState {
  error?: string;
  runId?: string;
  status?: string;
  awaitingApproval: boolean;
  interrupts?: Interrupt[];
  // Client tool calls the run left for this client to answer.
  pendingToolCallIds?: string[];
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
  resume?: ResumeEntry[];
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
  // Called when a posted turn was taken into a run already going on the
  // thread (the server answered 204), whose stream is followed instead.
  onFolded?: () => void;
}

/** Implement this interface to use a different backend without changing your UI. */
export interface ChatTransport {
  routines?: RoutineTransport;
  mcp?: MCPTransport;
  // Send a client tool's results as a turn while its run is still streaming,
  // without reading the run the turn may start: a live run takes them, and a
  // run that has ended is resumed by them and joined afterwards.
  sendToolResults?(
    agent: string,
    thread: string,
    input: RunInput,
    signal?: AbortSignal,
  ): Promise<void>;
  listSkills?(signal: AbortSignal): Promise<UserSkill[]>;
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
  skills: UserSkill[];
  skillSelection: SkillSelection;
  loadingSkills: boolean;
  skillsError: Error | null;
  mcpServers: MCPServer[];
  mcpSelection: MCPSelection;
  loadingMCPServers: boolean;
  mcpServersError: Error | null;
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
// A tool this client runs itself, such as reading the page or asking the
// browser for its time zone. Its definition is sent with every run this
// client starts. When the model calls it, the handler runs as soon as the call
// has streamed in: the result is posted to a run waiting for it, or, if the
// server pauses on the call instead, sent on a run that resumes it.
export interface ClientTool {
  name: string;
  description?: string;
  // JSON schema for the arguments; defaults to an empty object.
  parameters?: Record<string, unknown>;
  handler(
    args: any,
    context: { toolCallId: string; threadId: string; signal: AbortSignal },
  ): unknown;
}

export interface ChatOptions {
  agent: string;
  transport: ChatTransport;
  clientTools?: ClientTool[];
  groupId?: string;
  initialThreadId?: string;
  fullHistory?: boolean;
  watchRuns?: boolean;
  forwardedProps?: Record<string, unknown>;
  context?: RunInput["context"];
  onEvent?: (event: ChatEvent) => void;
  createId?: () => string;
}

import { skillUploadPath } from "./skill-upload";
// Thin client for the AG-UI endpoints served by pkg/agui. Everything
// is same-origin (the Go server serves both this UI and the API), so
// no auth headers or base URL config is needed. The embedded UI sends no
// namespace overrides: without a server resolver, every endpoint uses "default".
//
// The /messages endpoint already returns AG-UI-shaped messages
// (the Go handler converts stored history server-side), so there's no
// SDK→AG-UI conversion to do here — unlike the gateway demo, which
// converted on the client.

import type { Message as AGUIMessage } from "@ag-ui/core";

const API = "/api/agui";

export interface ThreadInfo {
  thread_id: string;
  conversation_id: string;
  namespace: string;
  title: string;
  message_count: number;
  created_at: string;
  updated_at: string;
}

// fetchAgents returns the registered agent names along with whether the
// server wants the client's whole message list posted on every run. It
// normally doesn't — the agent loads the thread itself — so the default
// on an older server that omits the field is the cheap one.
export async function fetchAgents(): Promise<{
  agents: string[];
  fullHistory: boolean;
  attachmentsEnabled: boolean;
  skillStoreEnabled: boolean;
}> {
  const r = await fetch(`${API}/agents`);
  if (!r.ok) throw new Error(`agents → ${r.status}`);
  const body = await r.json();
  return { agents: body.agents ?? [], fullHistory: body.full_history === true, attachmentsEnabled: body.attachments === true, skillStoreEnabled: body.skill_store === true };
}

// fetchThreads returns supported=false when the agent's persistence
// adapter can't enumerate threads (the endpoint answers 501), so the
// caller can hide the conversation picker.
export async function fetchThreads(
  agent: string
): Promise<{ supported: boolean; threads: ThreadInfo[] }> {
  const r = await fetch(`${API}/agents/${encodeURIComponent(agent)}/threads`);
  if (r.status === 501) return { supported: false, threads: [] };
  if (!r.ok) throw new Error(`threads → ${r.status}`);
  return { supported: true, threads: (await r.json()).threads ?? [] };
}

// ThreadRunState is what the thread's last run left outstanding. Absent means
// nothing is: a settled thread reports no run at all.
export interface ThreadRunState {
  runId?: string;
  status?: string;
  awaitingApproval: boolean;
  interrupts?: Record<string, unknown>[];
  pendingToolCalls?: Record<string, unknown>[];
  backgroundTasks?: ThreadBackgroundTask[];
}

export interface ThreadBackgroundTask {
  taskId: string;
  callId?: string;
  toolName?: string;
  streamId?: string;
  startedAt?: string;
}

// fetchMessages returns the thread's history and whatever its last run left
// outstanding.
//
// The run state matters on a reload. An approval card is drawn from an event
// only a live run emits, so a browser that refreshes while the agent waits for
// a decision would otherwise show nothing at all — the agent still waiting,
// and no way to answer it.
export async function fetchMessages(
  agent: string,
  threadId: string,
  cursor?: string,
  limit = 50
): Promise<{ messages: AGUIMessage[]; run: ThreadRunState | null; nextCursor: string; sessionId: string }> {
  const r = await fetch(
    `${API}/agents/${encodeURIComponent(agent)}/threads/${encodeURIComponent(
      threadId
    )}/messages?${new URLSearchParams({ limit: String(limit), ...(cursor ? { cursor } : {}) })}`
  );
  if (!r.ok) throw new Error(`messages → ${r.status}`);
  const body = await r.json();
  return { sessionId: body.sessionId ?? threadId, messages: body.messages ?? [], run: body.run ?? null, nextCursor: body.nextCursor ?? "" };
}

export function runUrl(agent: string): string {
  return new URL(
    `${API}/agents/${encodeURIComponent(agent)}/run`,
    window.location.origin
  ).toString();
}

// streamUrl is the thread's run stream: attaching to it replays the run
// so far and then follows it live, without starting a turn.
//
// waitSeconds asks the server to hold the request open for that long if no
// run is going yet, instead of answering 204 at once. A rejoin that follows a
// watch uses it: the watch reports the run the moment it claims the thread,
// and the run publishes its first chunk a beat later — without the wait, a
// rejoin landing in between is told there is nothing to join.
export function streamUrl(
  agent: string,
  threadId: string,
  waitSeconds = 0
): string {
  const url = new URL(
    `${API}/agents/${encodeURIComponent(agent)}/threads/${encodeURIComponent(
      threadId
    )}/stream`,
    window.location.origin
  );
  if (waitSeconds > 0) url.searchParams.set("wait", String(waitSeconds));
  return url.toString();
}

// stopRun asks the server to end a run in flight, identified by the
// thread in the server-resolved namespace. The optional stream ID checks
// that the request matches the stream being displayed. The server answers straight away;
// the run winds down on its own SSE connection and ends there with
// RUN_FINISHED, so keep reading that stream.
export async function stopRun(agent: string, threadId: string, streamId?: string): Promise<void> {
  const r = await fetch(`${API}/agents/${encodeURIComponent(agent)}/stop`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ threadId, streamId }),
    // A stop is worth delivering even if the user navigates away.
    keepalive: true,
  });
  if (!r.ok) throw new Error(`stop → ${r.status}`);
}

export function relativeTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const diff = Date.now() - d.getTime();
  const min = 60_000,
    hr = 3_600_000,
    day = 86_400_000;
  if (diff < min) return "just now";
  if (diff < hr) return `${Math.floor(diff / min)}m ago`;
  if (diff < day) return `${Math.floor(diff / hr)}h ago`;
  if (diff < 7 * day) return `${Math.floor(diff / day)}d ago`;
  return d.toLocaleDateString();
}

// RunFeedEvent is one run beginning or ending, anywhere in the namespaces
// being watched.
export interface RunFeedEvent {
  event: "RUN_STARTED" | "RUN_FINISHED";
  namespace: string;
  threadId: string;
  runId?: string;
  agentName?: string;
  streamId: string;
  at: string;
}

// watchRunFeed is the long poll that tells a browser something happened in a
// conversation it is not looking at.
//
// The per-thread watch cannot: it is keyed to one thread's channel, so a
// conversation that starts elsewhere — or one that did not exist when this
// page loaded — has nothing the browser could have been attached to. This
// watches the namespace resolved by the server, without a client override.
//
// The cursor is opaque and belongs to the server. Hand back what it last gave
// you and a run that started and ended while the tab was hidden is still
// reported; send nothing and the feed starts from now, which is what a page
// loading for the first time wants.
export async function watchRunFeed(
  agent: string,
  cursor: string,
  waitSeconds: number,
  signal?: AbortSignal
): Promise<{ events: RunFeedEvent[]; cursor: string }> {
  const url = new URL(
    `${API}/agents/${encodeURIComponent(agent)}/runs`,
    window.location.origin
  );
  url.searchParams.set("wait", String(waitSeconds));
  if (cursor) url.searchParams.set("cursor", cursor);

  const r = await fetch(url.toString(), { signal });
  if (r.status === 501) return { events: [], cursor };
  if (!r.ok) throw new Error(`runs → ${r.status}`);
  const body = await r.json();
  return { events: body.events ?? [], cursor: body.cursor ?? cursor };
}

export interface UploadedAttachment {
  file_id: string;
  url: string;
  filename: string;
  mediaType: string;
  size: number;
}

export async function uploadAttachment(file: File, sessionId: string): Promise<UploadedAttachment> {
  if (file.size > 20 * 1024 * 1024) throw new Error("Files must be 20 MiB or smaller.");
  const body = new FormData(); body.append("file", file);
  body.append("session_id", sessionId);
  const response = await fetch(`${API}/attachments/`, { method: "POST", body });
  if (!response.ok) throw new Error(`Upload failed (${response.status}). Please try again.`);
  return response.json();
}

export interface SkillInfo {
  name: string;
  description: string;
  required?: boolean;
  defaultEnabled?: boolean;
  global?: boolean;
  enabled: boolean;
}
export async function fetchSkills(agent: string): Promise<SkillInfo[]> {
  const r = await fetch(`${API}/agents/${encodeURIComponent(agent)}/skills`);
  if (!r.ok) throw new Error(`skills → ${r.status}`);
  return (await r.json()).skills ?? [];
}

export interface StoredSkill { name: string; description: string; resources: string[] }
export async function fetchStoredSkills(cursor = ""): Promise<{skills: StoredSkill[]; nextCursor?: string}> {
  const r = await fetch(`${API}/skills?limit=50&cursor=${encodeURIComponent(cursor)}`);
  if (!r.ok) throw new Error(await r.text());
  return r.json();
}
export function skillFileUrl(name: string, file: string): string {
  return `${API}/skills/${encodeURIComponent(name)}?file=${encodeURIComponent(file)}`;
}
export async function uploadSkill(files: File[]): Promise<StoredSkill> {
  const data = new FormData();
  for (const file of files) {
    // A folder selection includes its enclosing directory. Store only paths
    // inside that directory, preserving nested references and scripts.
    const path = skillUploadPath(file);
    data.append("files", file, path);
  }
  const r = await fetch(`${API}/skills`, { method: "POST", body: data });
  if (!r.ok) throw new Error(await r.text());
  return r.json();
}

export async function deleteSkill(name: string): Promise<void> {
  const r = await fetch(`${API}/skills/${encodeURIComponent(name)}`, { method: "DELETE" });
  if (!r.ok) throw new Error(await r.text());
}

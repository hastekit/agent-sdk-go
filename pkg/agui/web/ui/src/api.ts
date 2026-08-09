// Thin client for the AG-UI endpoints served by pkg/agui. Everything
// is same-origin (the Go server serves both this UI and the API), so
// no auth headers or base URL config is needed.
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

export async function fetchAgents(): Promise<string[]> {
  const r = await fetch(`${API}/agents`);
  if (!r.ok) throw new Error(`agents → ${r.status}`);
  return (await r.json()).agents ?? [];
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

export async function fetchMessages(
  agent: string,
  threadId: string
): Promise<AGUIMessage[]> {
  const r = await fetch(
    `${API}/agents/${encodeURIComponent(agent)}/threads/${encodeURIComponent(
      threadId
    )}/messages`
  );
  if (!r.ok) throw new Error(`messages → ${r.status}`);
  return (await r.json()).messages ?? [];
}

export function runUrl(agent: string): string {
  return new URL(
    `${API}/agents/${encodeURIComponent(agent)}/run`,
    window.location.origin
  ).toString();
}

// streamUrl is the thread's run stream: attaching to it replays the run
// so far and then follows it live, without starting a turn.
export function streamUrl(agent: string, threadId: string): string {
  return new URL(
    `${API}/agents/${encodeURIComponent(agent)}/threads/${encodeURIComponent(
      threadId
    )}/stream`,
    window.location.origin
  ).toString();
}

// stopRun asks the server to end a run in flight, identified by the
// stream id it reported at the start. The server answers straight away;
// the run winds down on its own SSE connection and ends there with
// RUN_FINISHED, so keep reading that stream.
export async function stopRun(agent: string, streamId: string): Promise<void> {
  const r = await fetch(`${API}/agents/${encodeURIComponent(agent)}/stop`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ streamId }),
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

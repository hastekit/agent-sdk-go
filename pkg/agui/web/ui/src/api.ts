import { skillUploadPath } from "./skill-upload";
// Thin client for the app's own endpoints served by pkg/agui: agents,
// threads, skills, routines and MCP servers. Everything the chat itself needs
// (runs, history, the run feed) comes from @hastekit/copilotkit. Everything is
// same-origin (the Go server serves both this UI and the API), so no auth
// headers or base URL config is needed. The embedded UI sends no namespace
// overrides: without a server resolver, every endpoint uses "default".

const API = "/api/agui";

export interface ThreadInfo {
  group_id?: string;
  agent_name?: string;
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
  mcpStoreEnabled: boolean;
  routinesEnabled: boolean;
}> {
  const r = await fetch(`${API}/agents`);
  if (!r.ok) throw new Error(`agents → ${r.status}`);
  const body = await r.json();
  return { routinesEnabled: body.routines === true, agents: body.agents ?? [], fullHistory: body.full_history === true, attachmentsEnabled: body.attachments === true, skillStoreEnabled: body.skill_store === true, mcpStoreEnabled: body.mcp_store === true };
}

// fetchThreads returns supported=false when the agent's persistence
// adapter can't enumerate threads (the endpoint answers 501), so the
// caller can hide the conversation picker.
export async function fetchThreads(
  agent: string,
  groupId = "default"
): Promise<{ supported: boolean; threads: ThreadInfo[] }> {
  const r = await fetch(`${API}/agents/${encodeURIComponent(agent)}/threads?${new URLSearchParams({ group_id: groupId })}`);
  if (r.status === 501) return { supported: false, threads: [] };
  if (!r.ok) throw new Error(`threads → ${r.status}`);
  return { supported: true, threads: (await r.json()).threads ?? [] };
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

// The user's own skills, shared by every agent and on unless turned off. Agents'
// global skills are configured on the server, always on, and never listed.
export interface SkillInfo {
  name: string;
  description: string;
}
export async function fetchSkillCatalog(): Promise<SkillInfo[]> {
  const skills: SkillInfo[] = [];
  const seen = new Set<string>();
  let cursor = "";
  for (;;) {
    const r = await fetch(`${API}/skills?limit=200&cursor=${encodeURIComponent(cursor)}`);
    if (r.status === 404) return []; // no skill store configured
    if (!r.ok) throw new Error(`skills → ${r.status}`);
    const page: { skills?: SkillInfo[]; nextCursor?: string } = await r.json();
    skills.push(...(page.skills ?? []));
    if (!page.nextCursor) return skills;
    if (seen.has(page.nextCursor)) throw new Error("skills → repeated pagination cursor");
    seen.add(page.nextCursor);
    cursor = page.nextCursor;
  }
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

export interface RoutineDefinition {
  name: string;
  agent: string;
  instruction: string;
  schedule: { at?: string; cron?: string; timezone?: string };
}
export interface Routine extends RoutineDefinition {
  id: string;
  enabled: boolean;
}
async function routineRequest(path = "", init?: RequestInit): Promise<any> {
  const response = await fetch(`${API}/routines${path}`, init);
  if (!response.ok) {
    const body = await response.json().catch(() => ({}));
    throw new Error(body.error || `Routines request failed (${response.status})`);
  }
  return response.json();
}
export async function fetchRoutines(): Promise<Routine[]> {
  return (await routineRequest()) ?? [];
}
export async function fetchRoutineAgents(): Promise<string[]> {
  return (await routineRequest("/agents")) ?? [];
}
export async function createRoutine(definition: RoutineDefinition): Promise<Routine> {
  return routineRequest("", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(definition) });
}
export async function setRoutineEnabled(id: string, enabled: boolean): Promise<Routine> {
  return routineRequest(`/${encodeURIComponent(id)}/${enabled ? "resume" : "pause"}`, { method: "POST" });
}

export async function fetchRoutineThreads(id: string): Promise<ThreadInfo[]> {
  const body = await routineRequest(`/${encodeURIComponent(id)}/threads`);
  return body.threads ?? [];
}
export async function fetchRoutine(id: string): Promise<Routine> {
  return routineRequest(`/${encodeURIComponent(id)}`);
}

export async function runRoutineNow(id: string): Promise<{ id: string }> {
  return routineRequest(`/${encodeURIComponent(id)}/run`, { method: "POST" });
}

// One MCP server the user can see. Global servers (no namespace) are the
// developer's: always on and read-only. The user's own are on unless turned off.
export interface MCPServerInfo {
  name: string;
  namespace?: string;
  transport?: string;
  readOnly: boolean;
  oauth: boolean;
  // Present when OAuth connect is available: whether this user has connected.
  connected?: boolean;
  error?: string;
}

// A user-owned remote server. Headers are literal (API keys); the server rejects
// templates, private addresses, and names taken by global servers.
export interface MCPServerConfig {
  endpoint: string;
  transport?: "streamable-http" | "sse";
  toolPrefix?: string;
  headers?: Record<string, string>;
  authorization?: {
    clientId: string;
    clientSecret?: string;
    authUrl: string;
    tokenUrl: string;
    redirectUrl: string;
    scopes?: string[];
  };
}

const mcpPath = (name: string) => `${API}/mcp/${encodeURIComponent(name)}`;

export function isGlobalMCPServer(server: MCPServerInfo): boolean {
  return !server.namespace;
}

export async function fetchMCPServers(): Promise<MCPServerInfo[]> {
  const r = await fetch(`${API}/mcp/`);
  if (r.status === 404) return []; // no MCP store configured
  if (!r.ok) throw new Error(`MCP servers → ${r.status}`);
  return (await r.json()) ?? [];
}

export async function saveMCPServer(name: string, config: MCPServerConfig): Promise<void> {
  const r = await fetch(mcpPath(name), { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(config) });
  if (!r.ok) throw new Error(await r.text());
}

export async function deleteMCPServer(name: string): Promise<void> {
  const r = await fetch(mcpPath(name), { method: "DELETE" });
  if (!r.ok) throw new Error(await r.text());
}

// Open in a new tab to sign in; the provider must redirect to mcpCallbackUrl.
export function mcpConnectUrl(name: string): string {
  return `${mcpPath(name)}/connect`;
}

export function mcpCallbackUrl(name: string): string {
  return new URL(`${mcpPath(name)}/callback`, window.location.href).toString();
}

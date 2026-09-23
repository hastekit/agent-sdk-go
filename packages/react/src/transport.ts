import type { Routine } from "./routines.js";
import { HTTPError, resumableStream } from "./stream.js";
import type {
  ChatTransport,
  AgentSkill,
  MessagePage,
  Thread,
  Attachment,
  RunFeedEvent,
} from "./types.js";

// Configure endpoint routing, authentication, and stream recovery independently of React.
export interface AGUITransportOptions {
  /** Relative URL in browsers, absolute URL for SSR/custom fetch implementations. */
  baseUrl?: string;
  headers?: HeadersInit | (() => HeadersInit | Promise<HeadersInit>);
  credentials?: RequestCredentials;
  fetch?: typeof fetch;
  pageSize?: number;
  retryDelayMs?: number;
  maxRetries?: number;
  idleTimeoutMs?: number;
}

// Implement the chat transport using the SDK server's public AG-UI endpoints.
export function createAGUITransport(
  options: AGUITransportOptions = {},
): ChatTransport {
  // Normalize the base path once and defer global fetch access until a request.
  const base = (options.baseUrl ?? "/api/agui").replace(/\/$/, "");
  const fetcher: typeof fetch = (...args) =>
    (options.fetch ?? globalThis.fetch)(...args);

  // Re-evaluate authentication headers so refreshed credentials work during reconnects.
  const headers = async () =>
    new Headers(
      typeof options.headers === "function"
        ? await options.headers()
        : options.headers,
    );

  // Encode user-provided identifiers before inserting them into endpoint paths.
  const agentPath = (agent: string) =>
    `${base}/agents/${encodeURIComponent(agent)}`;
  const threadPath = (agent: string, thread: string) =>
    `${agentPath(agent)}/threads/${encodeURIComponent(thread)}`;

  // Share authentication and error handling across non-streaming requests.
  async function request(url: string, init: RequestInit = {}) {
    const h = await headers();
    new Headers(init.headers).forEach((v, k) => h.set(k, v));

    // Let fetch provide multipart boundaries even when callers supply default JSON headers.
    if (init.body instanceof FormData) h.delete("Content-Type");

    // Send each request with the caller's cancellation signal and credential policy.
    const response = await fetcher(url, {
      ...init,
      headers: h,
      credentials: options.credentials,
    });

    // Leave capability detection to the endpoint-specific handler.
    if (!response.ok && response.status !== 501)
      throw new HTTPError(
        response.status,
        (await response.text()) || `HTTP ${response.status}`,
      );
    return response;
  }

  // Reuse rotating authentication and JSON errors for every routines operation.
  async function routineRequest<T>(
    path = "",
    method = "GET",
    body?: unknown,
    signal?: AbortSignal,
  ): Promise<T> {
    const response = await request(`${base}/routines${path}`, {
      method,
      signal,
      ...(body === undefined
        ? {}
        : {
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(body),
          }),
    });
    if (response.status === 501)
      throw new HTTPError(501, "Routine operation is not supported");
    return response.status === 204
      ? (undefined as T)
      : (response.json() as Promise<T>);
  }
  const routinePath = (id: string) => `/${encodeURIComponent(id)}`;

  // Expose semantic operations so controllers never need to construct URLs.
  return {
    // Routine definitions, scheduler operations, and occurrence history share one transport.
    routines: {
      list: async (signal) =>
        (await routineRequest<Routine[]>("", "GET", undefined, signal)) ?? [],
      agents: async (signal) =>
        (await routineRequest<string[]>("/agents", "GET", undefined, signal)) ??
        [],
      get: (id, signal) =>
        routineRequest(routinePath(id), "GET", undefined, signal),
      create: (definition, signal) =>
        routineRequest("", "POST", definition, signal),
      update: (id, definition, signal) =>
        routineRequest(routinePath(id), "PUT", definition, signal),
      delete: (id, signal) =>
        routineRequest(routinePath(id), "DELETE", undefined, signal),
      setEnabled: (id, enabled, signal) =>
        routineRequest(
          `${routinePath(id)}/${enabled ? "resume" : "pause"}`,
          "POST",
          undefined,
          signal,
        ),
      runNow: (id, signal) =>
        routineRequest(`${routinePath(id)}/run`, "POST", undefined, signal),
      status: (id, signal) =>
        routineRequest(`${routinePath(id)}/status`, "GET", undefined, signal),
      threads: async (id, signal) =>
        (
          await routineRequest<{ threads?: Thread[] }>(
            `${routinePath(id)}/threads`,
            "GET",
            undefined,
            signal,
          )
        ).threads ?? [],
    },

    // Load the authorized agent catalog with the same credentials as chat requests.
    async listSkills(agent, signal) {
      const response = await request(`${agentPath(agent)}/skills`, { signal });
      if (response.status === 501)
        throw new HTTPError(501, "Skill listing is not supported");
      const body = (await response.json()) as { skills?: AgentSkill[] };
      return body.skills ?? [];
    },

    // Fetch persisted sidebar rows for the selected conversation group.
    async listThreads(agent, group, signal) {
      const r = await request(
        `${agentPath(agent)}/threads?${new URLSearchParams({ group_id: group })}`,
        { signal },
      );

      // Represent unsupported capabilities explicitly instead of parsing an error body.
      if (r.status === 501) return { supported: false, threads: [] };

      // Supply compatibility defaults for older SDK servers.
      const body = (await r.json()) as { threads?: Thread[] };
      return { supported: true, threads: body.threads ?? [] };
    },

    // Load one chronological history page and its outstanding run state.
    async loadMessages(agent, thread, cursor, signal) {
      const query = new URLSearchParams({
        limit: String(options.pageSize ?? 50),
      });
      if (cursor) query.set("cursor", cursor);
      const r = await request(
        `${threadPath(agent, thread)}/messages?${query}`,
        { signal },
      );

      // Represent unsupported capabilities explicitly instead of parsing an error body.
      if (r.status === 501)
        throw new HTTPError(501, "Message history is not supported");

      // Supply compatibility defaults for older SDK servers.
      const body = (await r.json()) as Partial<MessagePage>;
      return {
        messages: body.messages ?? [],
        run: body.run ?? null,
        nextCursor: body.nextCursor ?? "",
        sessionId: body.sessionId ?? thread,
      };
    },

    // Start a turn or rejoin an existing run through one resumable pipeline.
    stream(agent, thread, input, streamOptions) {
      return resumableStream({
        ...options,
        ...streamOptions,
        fetch: fetcher,
        headers: async () => {
          const h = await headers();
          if (input) h.set("Content-Type", "application/json");
          return h;
        },
        initialURL: input
          ? `${agentPath(agent)}/run`
          : `${threadPath(agent, thread)}/stream${streamOptions.waitForRun ? "?wait=20" : ""}`,
        rejoinURL: `${threadPath(agent, thread)}/stream?wait=20`,
        body: input ? JSON.stringify(input) : undefined,
      });
    },

    // Cancel server work without closing the stream that reports its completion.
    async stop(agent, thread, streamId, signal) {
      const r = await request(`${agentPath(agent)}/stop`, {
        method: "POST",
        signal,
        keepalive: true,
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ threadId: thread, streamId }),
      });

      // Represent unsupported capabilities explicitly instead of parsing an error body.
      if (r.status === 501)
        throw new HTTPError(501, "Stopping runs is not supported");
    },

    // Long-poll lifecycle changes for conversations created outside this client.
    async watchRuns(agent, cursor, signal) {
      const query = new URLSearchParams({ wait: "25" });
      if (cursor) query.set("cursor", cursor);
      const r = await request(`${agentPath(agent)}/runs?${query}`, { signal });

      // Represent unsupported capabilities explicitly instead of parsing an error body.
      if (r.status === 501) return { supported: false, events: [], cursor };

      // Supply compatibility defaults for older SDK servers.
      const body = (await r.json()) as {
        events?: RunFeedEvent[];
        cursor?: string;
      };
      return {
        supported: true,
        events: body.events ?? [],
        cursor: body.cursor ?? cursor,
      };
    },

    // Store a file in the selected attachment session using multipart form data.
    async uploadAttachment(file, sessionId, signal) {
      const body = new FormData();
      body.append("file", file);
      body.append("session_id", sessionId);
      const r = await request(`${base}/attachments/`, {
        method: "POST",
        body,
        signal,
      });

      // Represent unsupported capabilities explicitly instead of parsing an error body.
      if (r.status === 501)
        throw new HTTPError(501, "Attachments are not supported");
      return r.json() as Promise<Attachment>;
    },
  };
}

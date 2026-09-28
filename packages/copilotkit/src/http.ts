import type { HastekitConnection } from "./types";

export const DEFAULT_BASE_URL = "/api/agui";

/** A HasteKit endpoint answered with an error status. */
export class HastekitHTTPError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "HastekitHTTPError";
  }
}

// The URLs of one agent's endpoints. Names are encoded here so callers never build paths.
export function endpoints(connection: HastekitConnection) {
  const base = (connection.baseUrl ?? DEFAULT_BASE_URL).replace(/\/$/, "");
  const agent = `${base}/agents/${encodeURIComponent(connection.agentName)}`;
  const thread = (threadId: string) => `${agent}/threads/${encodeURIComponent(threadId)}`;
  return {
    run: `${agent}/run`,
    stop: `${agent}/stop`,
    runs: `${agent}/runs`,
    messages: (threadId: string) => `${thread(threadId)}/messages`,
    // waitSeconds holds the request open until a run claims the thread, instead
    // of answering 204 at once; a rejoin racing a run's first chunk needs it.
    stream: (threadId: string, waitSeconds = 0) =>
      `${thread(threadId)}/stream${waitSeconds > 0 ? `?wait=${waitSeconds}` : ""}`,
  };
}

export function fetcher(connection: HastekitConnection) {
  return connection.fetch ?? ((url: string, init: RequestInit) => fetch(url, init));
}

// request sends one non-streaming request with the connection's headers and
// raises HastekitHTTPError for an error status other than those allowed.
export async function request(
  connection: HastekitConnection,
  url: string,
  init: RequestInit = {},
  allow: number[] = [],
): Promise<Response> {
  const response = await fetcher(connection)(url, {
    ...init,
    headers: { ...connection.headers, ...(init.headers as Record<string, string> | undefined) },
  });
  if (!response.ok && !allow.includes(response.status)) {
    const text = await response.text().catch(() => "");
    throw new HastekitHTTPError(response.status, text || `HTTP ${response.status}`);
  }
  return response;
}

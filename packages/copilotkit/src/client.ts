import { endpoints, request } from "./http";
import type { HastekitConnection, RunFeedEvent, ThreadPage } from "./types";

/**
 * Loads one page of a thread's history, oldest message first, with whatever
 * the thread's last run left outstanding. Without a cursor it is the latest
 * page; `nextCursor` loads the page before it.
 *
 * Pass the messages to `HastekitAgent` as `initialMessages`, and older pages to
 * `prependMessages`. The run state is how a page that loads while the agent
 * waits on a person learns what it is waiting for: those interrupts are
 * answered with `HastekitAgent.resume`.
 */
export async function fetchThreadMessages(
  connection: HastekitConnection,
  threadId: string,
  options: { cursor?: string; limit?: number; signal?: AbortSignal } = {},
): Promise<ThreadPage> {
  const query = new URLSearchParams({ limit: String(options.limit ?? 50) });
  if (options.cursor) query.set("cursor", options.cursor);
  const response = await request(connection, `${endpoints(connection).messages(threadId)}?${query}`, {
    signal: options.signal,
  });
  const body = (await response.json()) as Partial<ThreadPage>;
  return {
    messages: body.messages ?? [],
    run: body.run ?? null,
    context: body.context ?? null,
    nextCursor: body.nextCursor ?? "",
    sessionId: body.sessionId ?? threadId,
  };
}

/**
 * Long-polls the runs starting and finishing on the agent's threads, including
 * threads this client has never opened.
 *
 * Hand back the returned cursor on the next call so a run that started and
 * ended between polls is still reported; with no cursor the feed starts from
 * now. `supported` is false when the server cannot provide the feed.
 */
export async function watchRuns(
  connection: HastekitConnection,
  options: { cursor?: string; waitSeconds?: number; signal?: AbortSignal } = {},
): Promise<{ events: RunFeedEvent[]; cursor: string; supported: boolean }> {
  const cursor = options.cursor ?? "";
  const query = new URLSearchParams({ wait: String(options.waitSeconds ?? 25) });
  if (cursor) query.set("cursor", cursor);
  const response = await request(connection, `${endpoints(connection).runs}?${query}`, { signal: options.signal }, [501]);
  if (response.status === 501) return { events: [], cursor, supported: false };
  const body = (await response.json()) as { events?: RunFeedEvent[]; cursor?: string };
  return { events: body.events ?? [], cursor: body.cursor ?? cursor, supported: true };
}

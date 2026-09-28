import { fetchThreadMessages, type HastekitAgent } from "@hastekit/copilotkit";
import { useEffect, useRef, useState } from "react";

// Older pages are prepended without rebuilding the agent or disconnecting its run.
export function HistoryPager({ agent, initialCursor, children }: {
  agent: HastekitAgent; initialCursor: string; children: React.ReactNode;
}) {
  const [cursor, setCursor] = useState(initialCursor);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const busy = useRef(false);
  const alive = useRef(true);
  const scrollArea = useRef<HTMLElement | null>(null);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  const load = async () => {
    if (!cursor || busy.current) return;
    busy.current = true; setLoading(true); setError("");
    try {
      const page = await fetchThreadMessages(agent.connection, agent.threadId, { cursor });
      if (!alive.current) return;
      const el = scrollArea.current;
      const height = el?.scrollHeight ?? 0;
      const top = el?.scrollTop ?? 0;
      agent.prependMessages(page.messages);
      setCursor(page.nextCursor);
      requestAnimationFrame(() => requestAnimationFrame(() => {
        if (alive.current && el) el.scrollTop = top + el.scrollHeight - height;
      }));
    } catch (e) {
      if (alive.current) setError(String(e));
    } finally {
      busy.current = false;
      if (alive.current) setLoading(false);
    }
  };
  return <div className="chat-inner history-pager">
    {cursor && <div className="history-controls">
      <button disabled={loading} onClick={() => void load()}>{loading ? "Loading older messages…" : error ? "Retry loading older messages" : "Load older messages"}</button>
      {error && <span role="alert">{error}</span>}
    </div>}
    <div className="history-chat" onScrollCapture={e => {
      const el = e.target as HTMLElement;
      if (el.tagName === "TEXTAREA" || el.scrollHeight <= el.clientHeight) return;
      scrollArea.current = el;
      if (el.scrollTop < 80 && !error) void load();
    }}>{children}</div>
  </div>;
}

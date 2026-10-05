import { useCallback, useEffect, useRef, useState } from "react";
import { fetchThreads, type ThreadInfo } from "../api";

// useThreadList keeps the sidebar's conversations for the selected agent.
// `supported` is false when the agent's store cannot list threads.
export function useThreadList(agentName: string, setError: (message: string | null) => void) {
  const [threads, setThreads] = useState<ThreadInfo[]>([]);
  const [supported, setSupported] = useState(true);
  // A listing that lands after the user switched agent belongs to the old one.
  const listingAgent = useRef(agentName);
  listingAgent.current = agentName;

  const refresh = useCallback(async () => {
    if (!agentName) return;
    try {
      const res = await fetchThreads(agentName);
      if (listingAgent.current !== agentName) return;
      setError(null);
      setSupported(res.supported);
      setThreads(res.threads);
    } catch (e) {
      setError(String(e));
    }
  }, [agentName, setError]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  // Forgets the previous agent's answer until the new agent's listing arrives.
  const reset = useCallback(() => setSupported(true), []);

  return { threads, supported, refresh, reset };
}

import { useEffect, useState } from "react";
import { fetchAgentServers, type AgentServer } from "../api";

// useAgentServers loads the MCP servers the agent the chat is pointed at has,
// each with whether it is always on. null means the server did not say, and
// every server the MCP library lists applies, built-in ones always on.
export function useAgentServers(agentName: string): AgentServer[] | null {
  const [servers, setServers] = useState<AgentServer[] | null>(null);
  useEffect(() => {
    setServers(null);
    if (!agentName) return;
    let cancelled = false;
    // Informational only: a failure falls back to the library's servers.
    fetchAgentServers(agentName).then(loaded => { if (!cancelled) setServers(loaded); }).catch(() => {});
    return () => { cancelled = true; };
  }, [agentName]);
  return servers;
}

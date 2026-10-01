import { useEffect, useState } from "react";
import { fetchAgents } from "../api";

export interface ServerConfig {
  agents: string[];
  // Whether the server needs the full message list posted on every run.
  // False is both the default and the common case.
  fullHistory: boolean;
  attachmentsEnabled: boolean;
  skillStoreEnabled: boolean;
  mcpStoreEnabled: boolean;
  routinesEnabled: boolean;
}

const NOTHING: ServerConfig = {
  agents: [],
  fullHistory: false,
  attachmentsEnabled: false,
  skillStoreEnabled: false,
  mcpStoreEnabled: false,
  routinesEnabled: false,
};

// useServerConfig loads the registered agents and what the server supports,
// once, and holds the agent the chat is pointed at.
//
// preferredAgent is the agent the address bar asked for. A name the server no
// longer registers falls back to the first rather than erroring: the link is
// stale, not wrong, and an empty chat against a real agent is a better landing
// than a dead page.
export function useServerConfig(preferredAgent: string, onError: (message: string) => void) {
  const [config, setConfig] = useState<ServerConfig>(NOTHING);
  const [agentName, setAgentName] = useState("");

  useEffect(() => {
    fetchAgents()
      .then(({ agents, ...supported }) => {
        setConfig({ agents, ...supported });
        if (!agents.length) {
          onError("No agents registered on the server.");
          return;
        }
        setAgentName(agents.includes(preferredAgent) ? preferredAgent : agents[0]);
      })
      .catch((e) => onError(String(e)));
    // Loaded once: the preference is for arriving at a URL.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return { ...config, agentName, setAgentName };
}

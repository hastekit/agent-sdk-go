import { useFrontendTool } from "@copilotkit/react-core/v2";
import type { HastekitAgent } from "@hastekit/copilotkit";
import { useHastekitClientTools } from "@hastekit/copilotkit/react";

// ClientTools lets the agent run tools this page registers with CopilotKit
// (useFrontendTool) as soon as their calls stream in.
export function ClientTools({ agent, pendingToolCallIds }: { agent: HastekitAgent; pendingToolCallIds?: string[] }) {
  // Calls the thread was left waiting on are answered once the tools are bound.
  useHastekitClientTools(agent, { pendingToolCallIds });

  // A built-in browser tool: agents often need the user's local time and zone,
  // for example to schedule a routine, and only the browser knows them.
  useFrontendTool({
    name: "get_browser_context",
    description: "Get the user's time zone, locale and current local time from their browser.",
    handler: async () => ({
      timeZone: Intl.DateTimeFormat().resolvedOptions().timeZone,
      locale: navigator.language,
      localTime: new Date().toString(),
    }),
  }, []);
  return null;
}

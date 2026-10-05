import { useCallback, useEffect, useMemo, useState } from "react";
import { CopilotChat, CopilotKitProvider } from "@copilotkit/react-core/v2";
import type { InputContent } from "@ag-ui/core";
import { HastekitAgent } from "@hastekit/copilotkit";
import { fetchMCPServers, fetchSkillCatalog, isGlobalMCPServer, type MCPServerInfo } from "./api";
import { ToolResultLookup } from "./tool-results";
import { AttachmentMessageView } from "./attachment-message";
import { ComposerMCPContext, ComposerSkillsContext } from "./composer-menu";
import { MCPLibrary } from "./mcp-library";
import { RoutineLibrary } from "./routine-library";
import { SkillLibrary } from "./skill-library";
import { AGENT_PARAM, paramFromURL } from "./url-params";
import { AgentMenu } from "./components/AgentMenu";
import { ClientTools } from "./components/ClientTools";
import { SteerableInput, TrayContext } from "./components/ComposerTray";
import { HistoryPager } from "./components/HistoryPager";
import { InterruptHandler } from "./components/Interrupts";
import { RoutineHistory } from "./components/RoutineHistory";
import { Sidebar } from "./components/Sidebar";
import { InlineToolRenderer } from "./components/ToolCallCard";
import { useBuiltInSkills } from "./hooks/useBuiltInSkills";
import { useCatalogChoices } from "./hooks/useCatalogChoices";
import { useNavigation } from "./hooks/useNavigation";
import { useRoutineList } from "./hooks/useRoutineList";
import { useRunFeed } from "./hooks/useRunFeed";
import { useServerConfig } from "./hooks/useServerConfig";
import { useThreadActivity } from "./hooks/useThreadActivity";
import { useThreadList } from "./hooks/useThreadList";

// App drives the SDK's AG-UI endpoints through CopilotKit v2 and a
// HastekitAgent (@hastekit/copilotkit) registered via `selfManagedAgents`. A
// sidebar lists stored conversations (from the /threads endpoint) and
// resumes them by hydrating the agent with the thread's history.
//
// HITL goes through useHastekitInterrupt: the server finishes the run with an
// AG-UI 1.0 interrupt outcome, the hook hands us the open interrupts, and
// answering them fires a fresh run with the spec's resume entries.

// The user's own MCP servers can be turned off; the developer's global ones cannot.
const isUserMCPServer = (server: MCPServerInfo) => !isGlobalMCPServer(server);

export default function App() {
  const [error, setError] = useState<string | null>(null);
  const [preferredAgent] = useState(() => paramFromURL(AGENT_PARAM));
  const server = useServerConfig(preferredAgent, setError);
  const { agentName } = server;
  const threadList = useThreadList(agentName, setError);
  const nav = useNavigation({
    agentName,
    setAgentName: server.setAgentName,
    fullHistory: server.fullHistory,
    routinesEnabled: server.routinesEnabled,
    setError,
  });
  const { active, routine } = nav;

  // Fresh agent per (agent, thread). The provider re-keys on threadId
  // below so the whole chat subtree re-initialises cleanly when the user
  // switches conversations — no leaked in-flight stream or pending
  // interrupt from the prior thread.
  //
  // The thread's history goes in at construction. CopilotChat rejoins the
  // thread itself (it connects whenever it is given an explicit threadId, and
  // the server replays the run so far before following it live), and that
  // connect clears the agent's messages first — so hydrating from here
  // afterwards is a race we lose. HastekitAgent re-seeds its initial messages
  // when the connect starts instead.
  const agent = useMemo(() => {
    if (!agentName) return null;
    return new HastekitAgent({
      agentName,
      threadId: active.threadId,
      initialMessages: active.initialMessages,
      fullHistory: server.fullHistory,
    });
  }, [agentName, active.threadId, active.initialMessages, server.fullHistory]);

  // The user's own skills and MCP servers are shared by every agent: loaded
  // once, remembered once per browser, and sent with every run and resume.
  const skills = useCatalogChoices({ enabled: server.skillStoreEnabled, storageKey: "hastekit-skills", load: fetchSkillCatalog });
  // The agent's own skills: always on, shown beside the user's for reference.
  const builtInSkills = useBuiltInSkills(agentName);
  const mcp = useCatalogChoices({ enabled: server.mcpStoreEnabled, storageKey: "hastekit-mcp", load: fetchMCPServers, optional: isUserMCPServer });
  useEffect(() => {
    if (agent) agent.skillSelection = { disable: skills.disabled };
  }, [agent, skills.disabled]);
  useEffect(() => {
    if (agent) agent.mcpSelection = { disable: mcp.disabled };
  }, [agent, mcp.disabled]);

  const activity = useThreadActivity({
    agent,
    agentName,
    threadId: active.threadId,
    run: active.run,
    context: active.context,
    onRunActivity: threadList.refresh,
  });

  const routineList = useRoutineList(server.routinesEnabled);
  const { unseen, unseenRoutines } = useRunFeed(agentName, active.threadId, (events) => {
    // The opening user message is saved before RUN_STARTED, so new
    // conversations already have their title while the agent works.
    void threadList.refresh();
    for (const event of events) {
      if (event.event === "RUN_FINISHED" && event.groupId) routine.runFinished(event.groupId);
    }
    // A run on the conversation the user is reading is one to join, not
    // to badge: the answer belongs on screen as it is written.
    if (events.some((e) => e.event === "RUN_STARTED" && e.threadId === active.threadId)) {
      void agent?.joinIfIdle();
    }
  });

  const changeAgent = useCallback((name: string) => {
    nav.changeAgent(name);
    threadList.reset();
  }, [nav.changeAgent, threadList.reset]);

  // Steering: a turn typed while the agent is working folds into the run
  // in flight (see HastekitAgent.steer). Memoised so the composer
  // isn't remounted on every render.
  const steer = useCallback((text: string, parts: InputContent[] = []) => {
    const trimmed = text.trim();
    return agent?.steer(parts.length ? [...(trimmed ? [{ type: "text" as const, text: trimmed }] : []), ...parts] : trimmed);
  }, [agent]);
  // Cast: the slot type expects CopilotChatInput's own static sub-slots on
  // whatever it is handed. This wrapper only changes behaviour and renders
  // the real composer, so it has none of them and needs none.
  const inputSlot = useMemo(
    () => ((p: any) => <SteerableInput {...p} onSteer={steer} attachmentsEnabled={server.attachmentsEnabled} sessionId={active.sessionId} />) as any,
    [steer, server.attachmentsEnabled, active.sessionId]
  );

  // Tool cards find their stored result (and when it came back) by call id.
  const toolResultLookup = useCallback(
    (toolCallId: string) => agent?.messages.find((m) => m.role === "tool" && m.toolCallId === toolCallId),
    [agent]
  );

  const [routinesOpen, setRoutinesOpen] = useState(false);
  const [libraryOpen, setLibraryOpen] = useState(false);
  const [mcpLibraryOpen, setMCPLibraryOpen] = useState(false);

  return (
    // data-copilotkit + .dark put this whole tree in CopilotKit v2's
    // dark token scope (its tokens are defined on `[data-copilotkit].dark`).
    // The chat gets its own scope from CopilotKitProvider; setting it here
    // too means the sidebar — which lives OUTSIDE the provider — sees the
    // same --sidebar/--background/--border/... tokens and matches the chat.
    <div
      className="dark app"
      data-copilotkit
    >
      {routinesOpen && <RoutineLibrary initialAgent={agentName} onChanged={routineList.reload} onClose={() => setRoutinesOpen(false)} />}
      {libraryOpen && <SkillLibrary builtIn={builtInSkills} onClose={() => setLibraryOpen(false)} onSaved={skills.reload} />}
      {mcpLibraryOpen && <MCPLibrary onClose={() => setMCPLibraryOpen(false)} onChanged={mcp.reload} />}
      <Sidebar
        threads={threadList.threads}
        activeThreadId={active.threadId}
        unseen={unseen}
        unseenRoutines={unseenRoutines}
        onSelect={nav.selectThread}
        onNew={nav.startNewChat}
        selectedRoutineId={routine.selected?.id}
        onSelectRoutine={routine.select}
        routines={routineList.routines}
        routinesLoading={routineList.loading}
        routineError={routineList.error}
        onManageRoutines={server.routinesEnabled ? () => setRoutinesOpen(true) : undefined}
        onManageSkills={server.skillStoreEnabled ? () => setLibraryOpen(true) : undefined}
        onManageMCP={server.mcpStoreEnabled ? () => setMCPLibraryOpen(true) : undefined}
        onRetry={threadList.refresh}
        listingSupported={threadList.supported}
        error={error}
      />
      {routine.selected && !routine.threadOpen ? <main className="chat-pane routine-empty">
        <h2>{routine.selected.name}</h2>
        <p>{routine.loading ? "Loading latest conversation…" : routine.error || "This routine has no conversations yet. Its first run will appear here."}</p>
      </main> : agent && (
        <CopilotKitProvider
          key={active.threadId}
          selfManagedAgents={{ [agentName]: agent }}
          showDevConsole={false}
        >
          <div className="chat-pane">
            <header className="topbar">
              {routine.selected && <span className="routine-chat-title">{routine.selected.name}</span>}
              <AgentMenu
                agents={server.agents}
                agentName={agentName}
                onAgentChange={changeAgent}
              />
            </header>
            <InterruptHandler agentName={agentName} publish={activity.publishInterrupt} />
            <InlineToolRenderer agentName={agentName} />
            <ClientTools agent={agent} pendingToolCallIds={active.run?.pendingToolCallIds} />
            {activity.compacting && <div role="status" aria-live="polite" className="hint">Compacting context...</div>}
            {activity.runError && (
              <div className="run-error" role="alert">
                <span className="ico">⚠</span>
                <div className="msg">{activity.runError}</div>
                <button
                  className="dismiss"
                  onClick={activity.dismissRunError}
                  aria-label="Dismiss error"
                >
                  ×
                </button>
              </div>
            )}
            <ComposerSkillsContext.Provider value={{ builtIn: builtInSkills, skills: skills.items, choices: skills.choices, error: skills.error, toggle: skills.toggle, canManage: server.skillStoreEnabled, onManage: () => setLibraryOpen(true) }}>
            <ComposerMCPContext.Provider value={{ servers: mcp.items, choices: mcp.choices, error: mcp.error, toggle: mcp.toggle, canManage: server.mcpStoreEnabled, onManage: () => setMCPLibraryOpen(true) }}>
            <TrayContext.Provider value={activity.tray}>
              <ToolResultLookup.Provider value={toolResultLookup}>
              <HistoryPager key={`${agentName}:${active.threadId}`} agent={agent} initialCursor={active.nextCursor ?? ""}>
                <CopilotChat
                  agentId={agentName}
                  threadId={active.threadId}
                  labels={{
                    chatInputPlaceholder: "Ask anything",
                    chatDisclaimerText:
                      "The agent can make mistakes. Check important info.",
                  }}
                  input={inputSlot}
                  messageView={AttachmentMessageView as any}
                />
              </HistoryPager>
              </ToolResultLookup.Provider>
            </TrayContext.Provider>
            </ComposerMCPContext.Provider>
            </ComposerSkillsContext.Provider>
          </div>
        </CopilotKitProvider>
      )}
      {routine.selected && <RoutineHistory
        routine={routine.selected}
        threads={routine.threads}
        loading={routine.loading}
        error={routine.error}
        activeThreadId={routine.threadOpen ? active.threadId : ""}
        unseen={unseen}
        onManage={() => setRoutinesOpen(true)}
        onOpen={routine.openThread}
      />}
    </div>
  );
}

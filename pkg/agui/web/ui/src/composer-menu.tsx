import { createContext, useContext } from "react";
import * as Menu from "@radix-ui/react-dropdown-menu";
import { isGlobalMCPServer, type AgentServer, type MCPServerInfo, type SkillInfo } from "./api";

export const ComposerSkillsContext = createContext<{
  // The skills the agent always has — built in, or picked by its config, the
  // user's own among them — listed before the user's others.
  builtIn: SkillInfo[];
  // The user's skills the agent reads, by name; null when the server does not
  // say, and the whole library applies.
  optional: string[] | null;
  skills: SkillInfo[];
  choices: Record<string, boolean>;
  error: string;
  toggle: (name: string, enabled: boolean) => void;
  canManage: boolean;
  onManage: () => void;
}>({ builtIn: [], optional: null, skills: [], choices: {}, error: "", toggle: () => {}, canManage: false, onManage: () => {} });

export const ComposerMCPContext = createContext<{
  servers: MCPServerInfo[];
  // The agent's own servers, each with whether it is always on; null when the
  // server cannot say, and every listed server applies, built-in ones always on.
  agentServers: AgentServer[] | null;
  choices: Record<string, boolean>;
  error: string;
  toggle: (name: string, enabled: boolean) => void;
  canManage: boolean;
  onManage: () => void;
}>({ servers: [], agentServers: null, choices: {}, error: "", toggle: () => {}, canManage: false, onManage: () => {} });

// Only the agent's own servers are listed. Those always on can't be turned off:
// built-in ones, or ones the agent's config picks. The user's others are on
// unless turned off.
function MCPSubmenu() {
  const { servers: all, agentServers, choices, error, toggle, canManage, onManage } = useContext(ComposerMCPContext);
  const own = agentServers ? new Map(agentServers.map(server => [server.name, server.always_on])) : null;
  const servers = own ? all.filter(server => own.has(server.name)) : all;
  if (!canManage && !servers.length) return null;
  return <Menu.Sub>
    <Menu.SubTrigger className="composer-menu-item">MCP servers <span aria-hidden="true">›</span></Menu.SubTrigger>
    <Menu.Portal>
      <Menu.SubContent onClick={event => event.stopPropagation()} className="composer-menu composer-skills" sideOffset={6} collisionPadding={12}>
        <Menu.Label className="composer-skills-heading">MCP servers</Menu.Label>
        <p className="composer-skills-hint">Applies to the next run.</p>
        {error && <p className="composer-skills-hint" role="alert">{error}</p>}
        {!error && !servers.length && <p className="composer-skills-hint">{own ? "This agent uses no MCP servers." : "No MCP servers yet."}</p>}
        {servers.map(server => {
          const always = own ? own.get(server.name) === true : isGlobalMCPServer(server);
          const checked = always || (choices[server.name] ?? true);
          const problem = server.error ? "Needs fixing in Manage MCP servers" : server.connected === false ? "Not connected · connect in Manage MCP servers" : "";
          const note = always
            ? [isGlobalMCPServer(server) ? "Built in · always on" : "Yours · always on for this agent", problem].filter(Boolean).join(" · ")
            : problem;
          return <Menu.CheckboxItem key={server.name} className="composer-skill" role="switch"
            aria-label={server.name} checked={checked} disabled={always}
            onSelect={event => event.preventDefault()}
            onCheckedChange={enabled => toggle(server.name, enabled === true)}>
            <span className="composer-skill-copy"><strong>{server.name}</strong>
              {note && <small>{note}</small>}
            </span>
            <span className="skill-switch" data-checked={checked} aria-hidden="true"><span /></span>
          </Menu.CheckboxItem>;
        })}
        <Menu.Separator className="composer-menu-separator" />
        <Menu.Item asChild disabled={!canManage} onSelect={() => requestAnimationFrame(onManage)}>
          <button type="button" className="composer-menu-item composer-manage-skills" disabled={!canManage}>Manage MCP servers</button>
        </Menu.Item>
      </Menu.SubContent>
    </Menu.Portal>
  </Menu.Sub>;
}

// A stable slot component keeps the draft and uploaded files mounted when a
// skill choice changes. Radix supplies focus, keyboard and collision handling.
export function ComposerMenu({ onAddFile, disabled }: { onAddFile?: () => void; disabled?: boolean }) {
  const { builtIn, optional, skills: library, choices, error, toggle, canManage, onManage } = useContext(ComposerSkillsContext);
  // Only the agent's skills are listed. A skill of the user's that the agent
  // always has is listed with those, not as one they can turn off; one the
  // agent does not read is not listed at all.
  const always = new Set(builtIn.map(skill => skill.name));
  const reads = optional ? new Set(optional) : null;
  const yours = new Set(library.map(skill => skill.name));
  const skills = library.filter(skill => !always.has(skill.name) && (!reads || reads.has(skill.name)));
  return <Menu.Root>
    <Menu.Trigger asChild>
      <button type="button" className="composer-menu-trigger" disabled={disabled}
        aria-label="Attachments, skills and MCP servers" title="Attachments, skills and MCP servers">
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" aria-hidden="true"><path d="M12 5v14M5 12h14" /></svg>
      </button>
    </Menu.Trigger>
    <Menu.Portal>
      <Menu.Content onClick={event => event.stopPropagation()} className="composer-menu" side="top" align="start" sideOffset={8} collisionPadding={12}>
        {onAddFile && <>
          <Menu.Item className="composer-menu-item" onSelect={onAddFile}>Add attachment</Menu.Item>
          <Menu.Separator className="composer-menu-separator" />
        </>}
        <Menu.Sub>
          <Menu.SubTrigger className="composer-menu-item">Skills <span aria-hidden="true">›</span></Menu.SubTrigger>
          <Menu.Portal>
            <Menu.SubContent onClick={event => event.stopPropagation()} className="composer-menu composer-skills" sideOffset={6} collisionPadding={12}>
              <Menu.Label className="composer-skills-heading">Skills</Menu.Label>
              <p className="composer-skills-hint">Applies to the next run.</p>
              {error && <p className="composer-skills-hint" role="alert">{error}</p>}
              {builtIn.map(skill => <Menu.CheckboxItem key={`built-in:${skill.name}`} className="composer-skill" role="switch"
                aria-label={skill.name} checked disabled onSelect={event => event.preventDefault()}>
                <span className="composer-skill-copy"><strong>{skill.name}</strong><small>{yours.has(skill.name) ? "Yours · always on for this agent" : "Built in · always on"}</small></span>
                <span className="skill-switch" data-checked aria-hidden="true"><span /></span>
              </Menu.CheckboxItem>)}
              {!error && !skills.length && !builtIn.length && <p className="composer-skills-hint">{reads && !reads.size && library.length ? "This agent uses no skills." : canManage ? "Your skill library is empty." : "No skills of your own on this server."}</p>}
              {skills.map(skill => {
                const checked = choices[skill.name] ?? true;
                return <Menu.CheckboxItem key={skill.name} className="composer-skill" role="switch"
                  aria-label={skill.name} checked={checked}
                  onSelect={event => event.preventDefault()}
                  onCheckedChange={enabled => toggle(skill.name, enabled === true)}>
                  <span className="composer-skill-copy"><strong>{skill.name}</strong>
                  </span>
                  <span className="skill-switch" data-checked={checked} aria-hidden="true"><span /></span>
                </Menu.CheckboxItem>;
              })}
              <Menu.Separator className="composer-menu-separator" />
              <Menu.Item asChild disabled={!canManage} onSelect={() => {
                // Open after the menu closes and restores focus to its trigger.
                requestAnimationFrame(onManage);
              }}>
                <button type="button" className="composer-menu-item composer-manage-skills" disabled={!canManage}>Manage Skills</button>
              </Menu.Item>
              {!canManage && <p className="composer-skills-hint">Skill management is not configured on this server.</p>}
            </Menu.SubContent>
          </Menu.Portal>
        </Menu.Sub>
        <MCPSubmenu />
      </Menu.Content>
    </Menu.Portal>
  </Menu.Root>;
}

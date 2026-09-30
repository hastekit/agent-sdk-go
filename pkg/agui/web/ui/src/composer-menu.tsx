import { createContext, useContext } from "react";
import * as Menu from "@radix-ui/react-dropdown-menu";
import { isGlobalMCPServer, type MCPServerInfo, type SkillInfo } from "./api";

export const ComposerSkillsContext = createContext<{
  // The agent's own skills, always on, listed before the user's.
  builtIn: SkillInfo[];
  skills: SkillInfo[];
  choices: Record<string, boolean>;
  error: string;
  toggle: (name: string, enabled: boolean) => void;
  canManage: boolean;
  onManage: () => void;
}>({ builtIn: [], skills: [], choices: {}, error: "", toggle: () => {}, canManage: false, onManage: () => {} });

export const ComposerMCPContext = createContext<{
  servers: MCPServerInfo[];
  choices: Record<string, boolean>;
  error: string;
  toggle: (name: string, enabled: boolean) => void;
  canManage: boolean;
  onManage: () => void;
}>({ servers: [], choices: {}, error: "", toggle: () => {}, canManage: false, onManage: () => {} });

// The user's own servers are on unless turned off; built-in servers always are.
function MCPSubmenu() {
  const { servers, choices, error, toggle, canManage, onManage } = useContext(ComposerMCPContext);
  if (!canManage && !servers.length) return null;
  return <Menu.Sub>
    <Menu.SubTrigger className="composer-menu-item">MCP servers <span aria-hidden="true">›</span></Menu.SubTrigger>
    <Menu.Portal>
      <Menu.SubContent onClick={event => event.stopPropagation()} className="composer-menu composer-skills" sideOffset={6} collisionPadding={12}>
        <Menu.Label className="composer-skills-heading">MCP servers</Menu.Label>
        <p className="composer-skills-hint">Applies to the next run.</p>
        {error && <p className="composer-skills-hint" role="alert">{error}</p>}
        {!error && !servers.length && <p className="composer-skills-hint">No MCP servers yet.</p>}
        {servers.map(server => {
          const global = isGlobalMCPServer(server);
          const checked = global || (choices[server.name] ?? true);
          const note = global ? "Built in · always on" : server.error ? "Needs fixing in Manage MCP servers" : server.connected === false ? "Not connected · connect in Manage MCP servers" : "";
          return <Menu.CheckboxItem key={server.name} className="composer-skill" role="switch"
            aria-label={server.name} checked={checked} disabled={global}
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
  const { builtIn, skills, choices, error, toggle, canManage, onManage } = useContext(ComposerSkillsContext);
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
                <span className="composer-skill-copy"><strong>{skill.name}</strong><small>Built in · always on</small></span>
                <span className="skill-switch" data-checked aria-hidden="true"><span /></span>
              </Menu.CheckboxItem>)}
              {!error && !skills.length && !builtIn.length && <p className="composer-skills-hint">{canManage ? "Your skill library is empty." : "No skills of your own on this server."}</p>}
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

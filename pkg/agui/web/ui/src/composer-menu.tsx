import { createContext, useContext } from "react";
import * as Menu from "@radix-ui/react-dropdown-menu";
import type { SkillInfo } from "./api";

export const ComposerSkillsContext = createContext<{
  skills: SkillInfo[];
  choices: Record<string, boolean>;
  error: string;
  toggle: (name: string, enabled: boolean) => void;
  canManage: boolean;
  onManage: () => void;
}>({ skills: [], choices: {}, error: "", toggle: () => {}, canManage: false, onManage: () => {} });

// A stable slot component keeps the draft and uploaded files mounted when a
// skill choice changes. Radix supplies focus, keyboard and collision handling.
export function ComposerMenu({ onAddFile, disabled }: { onAddFile?: () => void; disabled?: boolean }) {
  const { skills, choices, error, toggle, canManage, onManage } = useContext(ComposerSkillsContext);
  return <Menu.Root>
    <Menu.Trigger asChild>
      <button type="button" className="composer-menu-trigger" disabled={disabled}
        aria-label="Attachments and skills" title="Attachments and skills">
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
              {!error && !skills.length && <p className="composer-skills-hint">No skills available for this agent.</p>}
              {skills.map(skill => {
                const required = skill.policy === "required";
                const checked = required || (choices[skill.name] ?? skill.enabled);
                return <Menu.CheckboxItem key={skill.name} className="composer-skill" role="switch"
                  aria-label={skill.name} checked={checked} disabled={required}
                  onSelect={event => event.preventDefault()}
                  onCheckedChange={enabled => toggle(skill.name, enabled === true)}>
                  <span className="composer-skill-copy"><strong>{skill.name}</strong>
                    {required && <small>Required · always enabled</small>}
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
      </Menu.Content>
    </Menu.Portal>
  </Menu.Root>;
}

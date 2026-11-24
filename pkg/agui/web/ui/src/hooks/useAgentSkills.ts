import { useEffect, useState } from "react";
import { fetchAgentSkills, type SkillInfo } from "../api";

// useAgentSkills loads the skills of the agent the chat is pointed at: those
// always on — built in, or picked by its config — and the names of the user's
// own it reads (null when the server does not say, and the whole library
// applies). Always-on skills are configured on the server, not in the library.
export function useAgentSkills(agentName: string): { builtIn: SkillInfo[]; optional: string[] | null } {
  const [skills, setSkills] = useState<{ builtIn: SkillInfo[]; optional: string[] | null }>({ builtIn: [], optional: null });
  useEffect(() => {
    setSkills({ builtIn: [], optional: null });
    if (!agentName) return;
    let cancelled = false;
    // Informational only: a failure falls back to the whole library.
    fetchAgentSkills(agentName).then(loaded => { if (!cancelled) setSkills({ builtIn: loaded.always, optional: loaded.optional }); }).catch(() => {});
    return () => { cancelled = true; };
  }, [agentName]);
  return skills;
}

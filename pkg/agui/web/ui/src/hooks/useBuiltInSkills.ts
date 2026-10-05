import { useEffect, useState } from "react";
import { fetchBuiltInSkills, type SkillInfo } from "../api";

// useBuiltInSkills loads the skills built in to the agent the chat is pointed
// at. They are configured on the server, always on, and never in the library.
export function useBuiltInSkills(agentName: string): SkillInfo[] {
  const [skills, setSkills] = useState<SkillInfo[]>([]);
  useEffect(() => {
    setSkills([]);
    if (!agentName) return;
    let cancelled = false;
    // Informational only: a failure leaves the list empty rather than blocking the chat.
    fetchBuiltInSkills(agentName).then(loaded => { if (!cancelled) setSkills(loaded); }).catch(() => {});
    return () => { cancelled = true; };
  }, [agentName]);
  return skills;
}

// What is on screen, in the address bar: the agent and the conversation.
//
// Without it a reload lands on the first registered agent in a brand-new empty
// chat, and what the user was looking at is only in the sidebar — which is no
// use at all when the agent is sitting on a question, since the prompt to
// answer it is on the thread they just lost. Both live in the URL rather than
// storage so a link carries the whole address, and so back/forward work.
//
// The thread belongs to the agent: a conversation is stored under the agent
// that held it, so the pair travels together and is cleared together.
export const AGENT_PARAM = "agent";
export const THREAD_PARAM = "thread";
export const ROUTINE_PARAM = "routine";

export function paramFromURL(name: string): string {
  return new URLSearchParams(window.location.search).get(name) ?? "";
}

// replaceState, not push: switching agent or conversation is not a navigation
// the back button should have to walk through.
export function rememberParam(name: string, value: string) {
  const url = new URL(window.location.href);
  if (value) url.searchParams.set(name, value);
  else url.searchParams.delete(name);
  window.history.replaceState(null, "", url.toString());
}

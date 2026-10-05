import { useEffect, useRef, useState } from "react";

// The title-bar dropdown: the agent the chat is pointed at. A menu rather
// than a <select> so it can carry the same weight as the rest of the bar —
// and so a single registered agent reads as a heading, not a control.
export function AgentMenu({
  agents,
  agentName,
  onAgentChange,
}: {
  agents: string[];
  agentName: string;
  onAgentChange: (name: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const wrap = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (!wrap.current?.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  const single = agents.length < 2;

  return (
    <div className="agent-menu" ref={wrap}>
      <button
        className="agent-trigger"
        onClick={() => !single && setOpen((v) => !v)}
        aria-haspopup={single ? undefined : "menu"}
        aria-expanded={single ? undefined : open}
        disabled={single}
      >
        {agentName || "No agent"}
        {!single && <ChevronIcon />}
      </button>

      {open && (
        <div className="agent-pop" role="menu">
          {agents.map((n) => (
            <button
              key={n}
              role="menuitem"
              className={"agent-opt" + (n === agentName ? " selected" : "")}
              onClick={() => {
                setOpen(false);
                if (n !== agentName) onAgentChange(n);
              }}
            >
              <span className="nm">{n}</span>
              {n === agentName && <CheckIcon />}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

// ── Icons ──────────────────────────────────────────────────

const svg = {
  width: 18,
  height: 18,
  viewBox: "0 0 24 24",
  fill: "none",
  stroke: "currentColor",
  strokeWidth: 1.6,
  strokeLinecap: "round" as const,
  strokeLinejoin: "round" as const,
};

function ChevronIcon() {
  return (
    <svg {...svg} width={16} height={16}>
      <path d="m6 9 6 6 6-6" />
    </svg>
  );
}

function CheckIcon() {
  return (
    <svg {...svg} width={16} height={16}>
      <path d="m5 13 4 4L19 7" />
    </svg>
  );
}

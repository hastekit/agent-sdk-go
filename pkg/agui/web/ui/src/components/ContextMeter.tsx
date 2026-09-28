import type { ContextUsage } from "@hastekit/copilotkit";

// ContextMeter shows how full the context window is, under the composer.
//
// The window is the agent's configured one (AgentConfig.ContextWindow): the
// SDK knows no model's, so without it the meter is just a count.
export function ContextMeter({ usage }: { usage: ContextUsage }) {
  const { tokens, window } = usage;

  if (!window) {
    return (
      <div className="context-meter" title={`${tokens.toLocaleString()} tokens in context`}>
        <span className="context-meter-label">{compact(tokens)} tokens in context</span>
      </div>
    );
  }

  const used = Math.min(tokens / window, 1);
  const percent = Math.round((tokens / window) * 100);
  const level = used >= 0.9 ? "critical" : used >= 0.75 ? "warn" : "ok";

  return (
    <div
      className="context-meter"
      data-level={level}
      title={`${tokens.toLocaleString()} of ${window.toLocaleString()} context window tokens used (${percent}%)`}
    >
      <div
        className="context-meter-bar"
        role="meter"
        aria-label="Context window used"
        aria-valuemin={0}
        aria-valuemax={window}
        aria-valuenow={Math.min(tokens, window)}
        aria-valuetext={`${percent}%`}
      >
        <div className="context-meter-fill" style={{ width: `${used * 100}%` }} />
      </div>
      <span className="context-meter-label">
        {compact(tokens)} / {compact(window)} · {percent}%
      </span>
    </div>
  );
}

// 1234 → "1.2k", 200000 → "200k", 1048576 → "1M".
function compact(n: number): string {
  if (n < 1000) return String(n);
  const [value, unit] = n < 1_000_000 ? [n / 1000, "k"] : [n / 1_000_000, "M"];
  return `${value >= 100 ? Math.round(value) : Number(value.toFixed(1))}${unit}`;
}

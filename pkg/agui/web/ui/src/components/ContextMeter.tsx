import type { ContextUsage } from "@hastekit/copilotkit";

// ContextMeter shows the current context token count under the composer.
export function ContextMeter({ usage }: { usage: ContextUsage }) {
  const { tokens } = usage;

  return (
    <div className="context-meter" title={`${tokens.toLocaleString()} tokens in context`}>
      <span className="context-meter-label">{compact(tokens)} tokens in context</span>
    </div>
  );
}

// 1234 → "1.2k", 200000 → "200k", 1048576 → "1M".
function compact(n: number): string {
  if (n < 1000) return String(n);
  const [value, unit] = n < 1_000_000 ? [n / 1000, "k"] : [n / 1_000_000, "M"];
  return `${value >= 100 ? Math.round(value) : Number(value.toFixed(1))}${unit}`;
}

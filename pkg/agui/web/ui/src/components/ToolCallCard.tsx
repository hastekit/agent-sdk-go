import { useDefaultRenderTool } from "@copilotkit/react-core/v2";

export function InlineToolRenderer({ agentName }: { agentName: string }) {
  useDefaultRenderTool(
    {
      render: (props: any) => <ToolCallCard {...props} />,
    },
    [agentName]
  );
  return null;
}

function ToolCallCard({
  name,
  status,
  parameters,
  result,
}: {
  name: string;
  status: "inProgress" | "executing" | "complete";
  parameters: unknown;
  result: string | undefined;
}) {
  const dot =
    status === "complete" ? "#10b981" : status === "executing" ? "#f59e0b" : "#94a3b8";
  const pill =
    status === "complete"
      ? { label: "Done", bg: "#dcfce7", fg: "#166534" }
      : status === "executing"
      ? { label: "Running", bg: "#fef3c7", fg: "#854d0e" }
      : { label: "Pending", bg: "#f1f5f9", fg: "#475569" };

  return (
    <div className="hk-tool">
      <details>
        <summary>
          <span className="hk-dot" style={{ background: dot }} />
          <code>{name}</code>
          <span className="hk-pill" style={{ background: pill.bg, color: pill.fg }}>
            {pill.label}
          </span>
        </summary>
        <div className="body">
          {hasContent(parameters) && <Block label="Arguments" value={parameters} />}
          {result && <Block label="Result" value={result} />}
        </div>
      </details>
    </div>
  );
}

function Block({ label, value }: { label: string; value: unknown }) {
  const text = typeof value === "string" ? value : safePretty(value);
  const clipped = text.length > 800 ? text.slice(0, 800) + "…" : text;
  return (
    <div>
      <div className="blk-label">{label}</div>
      <pre>{clipped}</pre>
    </div>
  );
}

function hasContent(v: unknown): boolean {
  if (v == null) return false;
  if (typeof v === "string") return v.length > 0;
  if (Array.isArray(v)) return v.length > 0;
  if (typeof v === "object") return Object.keys(v as object).length > 0;
  return true;
}

function safePretty(v: unknown): string {
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}

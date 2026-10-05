import {
  type FormField,
  type FormSchema,
  type HastekitInterrupt,
  type InterruptDecision,
} from "@hastekit/copilotkit";
import { useHastekitInterrupt } from "@hastekit/copilotkit/react";
import { useEffect, useRef, useState } from "react";
import type { TrayInterrupt } from "./ComposerTray";

// A run pauses in one of three ways, and each needs a different thing from
// the user: approve/reject a call, fill in a form, or visit a URL. They
// arrive as one AG-UI 1.0 interrupt outcome and can be mixed in one pause,
// so one card collects them all and answers each.

// InterruptHandler turns the live interrupt into a tray entry.
//
// useHastekitInterrupt renders where it is asked to, which is among the
// messages. That is one of the two places an approval used to appear, and the
// one that moved when the page was reloaded. So the render hands the pause
// upward and draws nothing itself; the tray decides where it goes.
export function InterruptHandler({
  agentName,
  publish,
}: {
  agentName: string;
  publish: (next: (current: TrayInterrupt | null) => TrayInterrupt | null) => void;
}) {
  useHastekitInterrupt({
    agentId: agentName,
    render: ({ interrupts, respond }) => (
      <InterruptPublisher interrupts={interrupts} submit={respond} publish={publish} />
    ),
  });
  return null;
}

// InterruptPublisher is the pause, held as state for as long as CopilotKit
// keeps it mounted, and drawn elsewhere.
//
// A component rather than a call in render because publishing is a state
// change, and the mount/unmount pair is exactly the lifetime the pause has.
function InterruptPublisher({
  interrupts,
  submit,
  publish,
}: {
  interrupts: HastekitInterrupt[];
  submit: (decisions: InterruptDecision[]) => void;
  publish: (next: (current: TrayInterrupt | null) => TrayInterrupt | null) => void;
}) {
  // submit is rebuilt on each of CopilotKit's renders; the tray holds one
  // callback for the life of the pause, so it reaches the current one here
  // instead of being republished to keep up.
  const submitRef = useRef(submit);
  useEffect(() => {
    submitRef.current = submit;
  });

  const key = interrupts.map((it) => it.id).join(",");
  useEffect(() => {
    publish(() => ({
      key,
      interrupts,
      onSubmit: (decisions) => submitRef.current(decisions),
    }));
    // Only if it is still ours: a pause resolved into another pause unmounts
    // this one after the next has already published, and a blind clear would
    // take the new one down with it.
    return () => publish((current) => (current?.key === key ? null : current));
    // interrupts is fixed for a given set of ids, which is what key is.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, publish]);

  return null;
}

// coerce turns a form input's string back into the type the schema asked
// for, so a number field does not arrive at the server quoted.
function coerce(raw: string, prop?: FormField): unknown {
  if (prop?.type === "number" || prop?.type === "integer") {
    if (raw.trim() === "") return undefined;
    const n = Number(raw);
    return Number.isNaN(n) ? raw : n;
  }
  return raw;
}

function initialForm(schema?: FormSchema): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [name, prop] of Object.entries(schema?.properties ?? {})) {
    if (prop.default !== undefined) out[name] = prop.default;
    else if (prop.type === "boolean") out[name] = false;
  }
  return out;
}

export function InterruptCard({
  interrupts,
  onSubmit,
}: {
  interrupts: HastekitInterrupt[];
  onSubmit: (decisions: InterruptDecision[]) => void;
}) {
  const approvals = interrupts.filter((it) => it.kind === "approval");
  const forms = interrupts.filter((it) => it.kind === "form");
  const urls = interrupts.filter((it) => it.kind === "url");

  const [checked, setChecked] = useState<Record<string, boolean>>(() => {
    const d: Record<string, boolean> = {};
    for (const it of approvals) d[it.id] = true;
    return d;
  });
  const [values, setValues] = useState<Record<string, Record<string, unknown>>>(() => {
    const d: Record<string, Record<string, unknown>> = {};
    for (const it of forms) d[it.id] = initialForm(it.schema);
    return d;
  });

  // A required field left empty would be rejected by the server's schema
  // validation, so catch it here where the user can still see the field.
  const missing = forms.some((it) =>
    (it.schema?.required ?? []).some((name) => {
      const v = values[it.id]?.[name];
      return v === undefined || v === "";
    })
  );

  const submit = (approved: boolean) =>
    onSubmit(
      interrupts.map((it) => {
        if (!approved) return { id: it.id, approved: false };
        if (it.kind === "form") return { id: it.id, approved: true, values: values[it.id] ?? {} };
        if (it.kind === "url") return { id: it.id, approved: true };
        return { id: it.id, approved: checked[it.id] ?? true };
      })
    );

  const title =
    forms.length || urls.length
      ? forms.length && !urls.length
        ? "The agent needs some details"
        : "The agent needs something from you"
      : `Approve ${approvals.length} pending tool call${approvals.length === 1 ? "" : "s"}`;

  return (
    <div className="hk-approval">
      <h4>⏸ {title}</h4>

      {approvals.map((it) => (
        <label className="hk-call" key={it.id}>
          <input
            type="checkbox"
            checked={checked[it.id] ?? true}
            onChange={(ev) =>
              setChecked((p) => ({ ...p, [it.id]: ev.target.checked }))
            }
          />
          <div className="meta">
            <div className="nm">{it.toolName}</div>
            <div className="args" title={it.arguments}>
              {it.arguments}
            </div>
          </div>
        </label>
      ))}

      {forms.map((it) => (
        <div className="hk-elicit" key={it.id}>
          {it.message && <p className="hk-elicit-msg">{it.message}</p>}
          {Object.entries(it.schema?.properties ?? {}).map(([name, prop]) => {
            const required = (it.schema?.required ?? []).includes(name);
            const value = values[it.id]?.[name];
            const set = (v: unknown) =>
              setValues((p) => ({
                ...p,
                [it.id]: { ...(p[it.id] ?? {}), [name]: v },
              }));
            return (
              <label className="hk-field" key={name}>
                <span className="hk-field-label">
                  {prop.title || name}
                  {required && <em className="hk-req"> *</em>}
                </span>
                {prop.enum ? (
                  <select
                    value={String(value ?? "")}
                    onChange={(ev) => set(ev.target.value)}
                  >
                    <option value="">—</option>
                    {prop.enum.map((opt) => (
                      <option key={opt} value={opt}>
                        {opt}
                      </option>
                    ))}
                  </select>
                ) : prop.type === "boolean" ? (
                  <input
                    type="checkbox"
                    checked={Boolean(value)}
                    onChange={(ev) => set(ev.target.checked)}
                  />
                ) : (
                  <input
                    type={prop.type === "number" || prop.type === "integer" ? "number" : "text"}
                    value={value === undefined ? "" : String(value)}
                    onChange={(ev) => set(coerce(ev.target.value, prop))}
                  />
                )}
                {prop.description && <span className="hk-hint">{prop.description}</span>}
              </label>
            );
          })}
        </div>
      ))}

      {urls.map((it) => (
        <div className="hk-elicit" key={it.id}>
          {it.message && <p className="hk-elicit-msg">{it.message}</p>}
          <a className="hk-link" href={it.url} target="_blank" rel="noreferrer noopener">
            {it.url}
          </a>
          <p className="hk-hint">Open the link, then continue below.</p>
        </div>
      ))}

      <div className="hk-actions">
        {approvals.length > 0 && !forms.length && !urls.length && (
          <span className="count">
            {Object.values(checked).filter(Boolean).length} of {approvals.length} approved
          </span>
        )}
        <button className="hk-btn" onClick={() => submit(false)}>
          {forms.length || urls.length ? "Cancel" : "Reject all"}
        </button>
        <button className="hk-btn primary" disabled={missing} onClick={() => submit(true)}>
          {forms.length || urls.length ? "Continue" : "Submit"}
        </button>
      </div>
    </div>
  );
}

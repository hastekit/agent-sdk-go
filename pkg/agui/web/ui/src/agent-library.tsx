import { useEffect, useRef, useState, type FormEvent } from "react";
import {
  deleteAgentConfig, fetchAgentChoices, fetchAgentConfigs, saveAgentConfig,
  type AgentChoice, type AgentChoices, type AgentConfig,
} from "./api";

// The form as edited: picks as sets, numbers and JSON as the text typed.
interface Form {
  name: string;
  description: string;
  model: string;
  instruction: string;
  tools: Set<string>;
  skills: Set<string>;
  // Picked connectors, each with the tools to keep typed as a comma list;
  // an empty list keeps every tool.
  connectors: Map<string, string>;
  handoffs: Set<string>;
  sticky: boolean;
  subAgents: Set<string>;
  maxLoops: string;
  temperature: string;
  output: string;
}

const NAME_RULE = /^[^\s/\\.][^/\\]{0,127}$/;

function emptyForm(choices: AgentChoices | null): Form {
  return {
    name: "", description: "", model: choices?.models[0] ?? "", instruction: "",
    tools: new Set(), skills: new Set(), connectors: new Map(), handoffs: new Set(),
    sticky: false, subAgents: new Set(), maxLoops: "", temperature: "", output: "",
  };
}

function formFrom(config: AgentConfig): Form {
  return {
    name: config.name,
    description: config.description ?? "",
    model: config.model,
    instruction: config.instruction ?? "",
    tools: new Set(config.tools ?? []),
    skills: new Set(config.skills ?? []),
    connectors: new Map(Object.entries(config.mcp ?? {}).map(([name, filter]) => [name, (filter.include ?? []).join(", ")])),
    handoffs: new Set((config.handoffs ?? []).map((h) => h.name)),
    sticky: config.sticky_handoff === true,
    subAgents: new Set((config.sub_agents ?? []).map((s) => s.name)),
    maxLoops: config.max_loops ? String(config.max_loops) : "",
    temperature: config.parameters?.temperature !== undefined ? String(config.parameters.temperature) : "",
    output: config.output ? JSON.stringify(config.output, null, 2) : "",
  };
}

// configFrom turns the form into the PUT body, leaving out what is empty.
// previous keeps fields this form does not edit, such as other parameters.
function configFrom(form: Form, choices: AgentChoices, previous?: AgentConfig): AgentConfig {
  const name = form.name.trim();
  const config: AgentConfig = { ...previous, name, model: form.model.trim() };
  const optional = <K extends keyof AgentConfig>(key: K, value: AgentConfig[K] | undefined) => {
    if (value === undefined || (Array.isArray(value) && !value.length) || value === "") delete config[key];
    else config[key] = value;
  };
  optional("description", form.description.trim());
  optional("instruction", form.instruction.trim());
  optional("tools", [...form.tools].sort());
  optional("skills", [...form.skills].sort());
  const mcp = Object.fromEntries([...form.connectors].map(([connector, tools]) => {
    const include = tools.split(/[\s,]+/).filter(Boolean);
    return [connector, include.length ? { include } : {}];
  }));
  optional("mcp", Object.keys(mcp).length ? mcp : undefined);
  const describe = new Map(choices.agents.map((a) => [a.name, a.description]));
  optional("handoffs", [...form.handoffs].filter((target) => target !== name).sort()
    .map((target) => ({ name: target, ...(describe.get(target) ? { description: describe.get(target) } : {}) })));
  optional("sticky_handoff", form.handoffs.size > 0 && form.sticky ? true : undefined);
  // A sub-agent kept from before keeps the settings this form does not edit.
  const keptSubAgents = new Map((previous?.sub_agents ?? []).map((s) => [s.name, s]));
  optional("sub_agents", [...form.subAgents].filter((target) => target !== name).sort()
    .map((target) => keptSubAgents.get(target) ?? { name: target }));

  const maxLoops = form.maxLoops.trim();
  if (maxLoops && (!Number.isInteger(Number(maxLoops)) || Number(maxLoops) < 1)) throw new Error("Max loops must be a whole number above zero.");
  optional("max_loops", maxLoops ? Number(maxLoops) : undefined);

  const parameters = { ...previous?.parameters };
  const temperature = form.temperature.trim();
  if (temperature) {
    const value = Number(temperature);
    if (!Number.isFinite(value) || value < 0 || value > 2) throw new Error("Temperature must be a number from 0 to 2.");
    parameters.temperature = value;
  } else delete parameters.temperature;
  optional("parameters", Object.keys(parameters).length ? parameters : undefined);

  const output = form.output.trim();
  if (output) {
    let schema: unknown;
    try { schema = JSON.parse(output); } catch { throw new Error("The output schema is not valid JSON."); }
    if (!schema || typeof schema !== "object" || Array.isArray(schema)) throw new Error("The output schema must be a JSON object.");
    config.output = schema as Record<string, unknown>;
  } else delete config.output;
  return config;
}

function summary(config: AgentConfig): string {
  const parts = [config.model];
  const count = (n: number, one: string) => n && parts.push(`${n} ${one}${n === 1 ? "" : "s"}`);
  count(config.tools?.length ?? 0, "tool");
  count(config.skills?.length ?? 0, "skill");
  count(Object.keys(config.mcp ?? {}).length, "connector");
  count(config.handoffs?.length ?? 0, "handoff");
  count(config.sub_agents?.length ?? 0, "sub-agent");
  return parts.join(" · ");
}

// AgentLibrary lets the user create, edit and delete agents of their own.
// What they pick from — models, tools, skills, connectors, other agents — is
// what the server offers; the server checks every save again.
export function AgentLibrary({ current, onClose, onChanged, onChat }: {
  current: string;
  onClose: () => void;
  // Called after a save or delete, so the agent list can refresh.
  onChanged: () => Promise<void>;
  // Opens a chat with the named agent and closes the dialog.
  onChat: (name: string) => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [configs, setConfigs] = useState<AgentConfig[]>([]);
  const [choices, setChoices] = useState<AgentChoices | null>(null);
  const [editing, setEditing] = useState<AgentConfig | null>(null);
  const [form, setForm] = useState<Form>(() => emptyForm(null));
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState<{ text: string; agent?: string } | null>(null);
  const [pendingDelete, setPendingDelete] = useState<string | null>(null);

  async function load() {
    setLoading(true); setError("");
    try {
      const [own, offered] = await Promise.all([fetchAgentConfigs(), fetchAgentChoices()]);
      setConfigs(own);
      setChoices(offered);
      setForm((f) => (f.model || editing ? f : { ...f, model: offered.models[0] ?? "" }));
    } catch (err) { setError(String(err)); }
    finally { setLoading(false); }
  }
  useEffect(() => { dialog.current?.showModal(); void load(); }, []);

  function edit(config: AgentConfig) {
    setEditing(config); setForm(formFrom(config)); setNotice(null); setError("");
    dialog.current?.querySelector(".agent-form")?.scrollIntoView({ behavior: "smooth", block: "start" });
  }
  function reset() { setEditing(null); setForm(emptyForm(choices)); }

  async function save(event: FormEvent, chat: boolean) {
    event.preventDefault();
    if (!choices) return;
    setError(""); setNotice(null);
    const name = form.name.trim();
    if (!NAME_RULE.test(name)) { setError("Give the agent a name of up to 128 characters, without slashes."); return; }
    if (!form.model.trim()) { setError("Choose a model."); return; }
    if (!editing && configs.some((c) => c.name === name)) { setError(`You already have an agent named ${name}. Edit it instead.`); return; }
    let config: AgentConfig;
    try { config = configFrom(form, choices, editing ?? undefined); }
    catch (err) { setError(err instanceof Error ? err.message : String(err)); return; }
    setBusy(true);
    try {
      const saved = await saveAgentConfig(config);
      await onChanged();
      if (chat) { onChat(saved.name); return; }
      setNotice({ text: `${editing ? "Saved" : "Created"} ${saved.name}.`, agent: saved.name });
      reset();
      await load();
    } catch (err) { setError(err instanceof Error ? err.message : String(err)); }
    finally { setBusy(false); }
  }

  async function remove(name: string) {
    setBusy(true); setError(""); setNotice(null);
    try {
      await deleteAgentConfig(name);
      setPendingDelete(null);
      if (editing?.name === name) reset();
      await onChanged();
      setNotice({ text: `Deleted ${name}.` });
      await load();
    } catch (err) { setError(err instanceof Error ? err.message : String(err)); }
    finally { setBusy(false); }
  }

  const toggle = (key: "tools" | "skills" | "handoffs" | "subAgents", name: string) => setForm((f) => {
    const next = new Set(f[key]);
    if (next.has(name)) next.delete(name); else next.add(name);
    return { ...f, [key]: next };
  });
  const toggleConnector = (name: string) => setForm((f) => {
    const next = new Map(f.connectors);
    if (next.has(name)) next.delete(name); else next.set(name, "");
    return { ...f, connectors: next };
  });
  const setConnectorTools = (name: string, tools: string) => setForm((f) => ({ ...f, connectors: new Map(f.connectors).set(name, tools) }));

  const handoffTargets = (choices?.agents ?? []).filter((a) => a.name !== form.name.trim());

  return <dialog ref={dialog} className="skill-library agent-library" onCancel={onClose} aria-labelledby="agent-library-title">
    <header>
      <div>
        <h2 id="agent-library-title">Agents</h2>
        <p>Build agents from the models, tools, skills and connectors this server offers. Only you can see and talk to the agents you create.</p>
      </div>
      <button onClick={onClose}>Close</button>
    </header>
    {error && <p role="alert" className="skill-library-error">{error}</p>}
    {notice && <p role="status" className="skill-library-notice">
      {notice.text}{" "}
      {notice.agent && <button className="link-button" onClick={() => onChat(notice.agent!)}>Chat with {notice.agent}</button>}
    </p>}

    <section aria-label="Your agents">
      <h3>Your agents <span className="skill-count">{configs.length}</span></h3>
      {loading ? <p role="status">Loading agents…</p> : !configs.length && <p className="skill-empty">You have no agents yet. Create one below.</p>}
      {configs.map((config) => <article key={config.name} className={editing?.name === config.name ? "selected" : ""}>
        <div className="skill-summary">
          <strong>{config.name}{config.name === current && <span className="badge">In chat</span>}</strong>
          {config.description && <p>{config.description}</p>}
          <small>{summary(config)}</small>
        </div>
        <div className="skill-row-actions">
          {pendingDelete === config.name ? <div className="skill-delete-confirm" role="group" aria-label={`Confirm deletion of ${config.name}`}>
            <p>Delete this agent? Its conversations stay in history.</p>
            <button disabled={busy} onClick={() => setPendingDelete(null)}>Cancel</button>
            <button className="skill-danger" disabled={busy} onClick={() => void remove(config.name)}>Confirm delete</button>
          </div> : <>
            <button disabled={busy} onClick={() => onChat(config.name)} aria-label={`Chat with ${config.name}`}>Chat</button>
            <button disabled={busy} onClick={() => edit(config)} aria-label={`Edit ${config.name}`}>Edit</button>
            <button className="skill-danger" disabled={busy} onClick={() => setPendingDelete(config.name)} aria-label={`Delete ${config.name}`}>Delete</button>
          </>}
        </div>
      </article>)}
    </section>

    <form className="agent-form" onSubmit={(e) => void save(e, false)}>
      <fieldset disabled={busy || loading || !choices} className="routine-form">
        <legend>{editing ? `Edit ${editing.name}` : "Create an agent"}</legend>
        <label>Name
          <input required value={form.name} disabled={!!editing} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="research-assistant" />
          {editing && <small>An agent keeps its name. Create a new agent to use another.</small>}
        </label>
        <label>Description<input value={form.description} onChange={(e) => setForm({ ...form, description: e.target.value })} placeholder="What this agent is for" /></label>
        <label>Model
          {choices?.models.length
            ? <select required value={form.model} onChange={(e) => setForm({ ...form, model: e.target.value })}>
                {!choices.models.includes(form.model) && form.model && <option value={form.model}>{form.model} (not offered)</option>}
                {choices.models.map((m) => <option key={m} value={m}>{m}</option>)}
              </select>
            : <input required value={form.model} onChange={(e) => setForm({ ...form, model: e.target.value })} placeholder="Provider/model" />}
        </label>
        <label>Instructions
          <textarea rows={5} value={form.instruction} onChange={(e) => setForm({ ...form, instruction: e.target.value })} placeholder="You are a helpful assistant that…" />
          <small>The system prompt. Skills, handoffs and connectors the agent can use are added to it automatically.</small>
        </label>

        <ChoiceGroup title="Tools" empty="This server offers no tools." items={choices?.tools ?? []} picked={form.tools} onToggle={(n) => toggle("tools", n)} />
        <ChoiceGroup title="Skills" empty="No skills are available. Upload one in the skill library." items={choices?.skills ?? []} picked={form.skills} onToggle={(n) => toggle("skills", n)} showOwner
          hint="Always on for this agent. Your other skills are on too, unless you turn them off in a chat." />

        <fieldset className="choice-group">
          <legend>MCP connectors</legend>
          <small>Always on for this agent. Your other connectors are on too, unless you turn them off in a chat.</small>
          {!choices?.connectors.length && <small>No connectors are available.</small>}
          <div className="choice-grid">
            {choices?.connectors.map((c) => <div key={c.name} className={"choice" + (form.connectors.has(c.name) ? " picked" : "")}>
              <label className="choice-label">
                <input type="checkbox" checked={form.connectors.has(c.name)} onChange={() => toggleConnector(c.name)} />
                <span><span className="nm">{c.name}</span>{c.builtIn ? <small>Built in</small> : <small>Yours</small>}</span>
              </label>
              {form.connectors.has(c.name) && <input className="choice-tools" value={form.connectors.get(c.name)}
                onChange={(e) => setConnectorTools(c.name, e.target.value)} placeholder="All tools, or: search, get_issue"
                aria-label={`Tools to use from ${c.name}`} />}
            </div>)}
          </div>
        </fieldset>

        <ChoiceGroup title="Handoffs" empty="There are no other agents to hand off to." items={handoffTargets} picked={form.handoffs} onToggle={(n) => toggle("handoffs", n)} showOwner />
        {form.handoffs.size > 0 && <label className="choice-label inline">
          <input type="checkbox" checked={form.sticky} onChange={(e) => setForm({ ...form, sticky: e.target.checked })} />
          <span>Keep talking to the agent it handed off to on the next turn</span>
        </label>}

        <ChoiceGroup title="Sub-agents" empty="There are no other agents to call." items={handoffTargets} picked={form.subAgents} onToggle={(n) => toggle("subAgents", n)} showOwner
          hint="Agents this one can call as tools. It keeps the conversation, sends them what they need, and gets their answers back." />

        <details className="agent-advanced">
          <summary>Advanced</summary>
          <label>Max loops<input type="number" min={1} step={1} value={form.maxLoops} onChange={(e) => setForm({ ...form, maxLoops: e.target.value })} placeholder="50" /></label>
          <label>Temperature<input type="number" min={0} max={2} step={0.1} value={form.temperature} onChange={(e) => setForm({ ...form, temperature: e.target.value })} placeholder="Model default" /></label>
          <label>Output schema (JSON)
            <textarea rows={4} value={form.output} onChange={(e) => setForm({ ...form, output: e.target.value })} placeholder='{"type":"object","properties":{"answer":{"type":"string"}}}' />
            <small>When set, the agent's final answer follows this JSON schema.</small>
          </label>
        </details>

        <div className="agent-form-actions">
          {editing && <button type="button" onClick={reset}>Cancel edit</button>}
          <button type="submit">{busy ? "Saving…" : editing ? "Save changes" : "Create agent"}</button>
          <button type="button" className="primary" onClick={(e) => void save(e as unknown as FormEvent, true)}>{editing ? "Save and chat" : "Create and chat"}</button>
        </div>
      </fieldset>
    </form>
  </dialog>;
}

function ChoiceGroup({ title, empty, items, picked, onToggle, showOwner = false, hint }: {
  title: string; empty: string; items: AgentChoice[]; picked: Set<string>; onToggle: (name: string) => void;
  // Explains the group, under its title.
  hint?: string;
  // Whether to say which items are built in and which are the user's own.
  showOwner?: boolean;
}) {
  // A pick the server no longer offers stays visible, so saving does not
  // silently drop it and the user can remove it deliberately.
  const missing = [...picked].filter((name) => !items.some((item) => item.name === name)).map((name) => ({ name, description: "No longer offered by the server", gone: true }));
  const all: (AgentChoice & { gone?: boolean })[] = [...items, ...missing];
  return <fieldset className="choice-group">
    <legend>{title}{picked.size > 0 && <span className="skill-count">{picked.size}</span>}</legend>
    {hint && <small>{hint}</small>}
    {!all.length && <small>{empty}</small>}
    <div className="choice-grid">
      {all.map((item) => <label key={item.name} className={"choice choice-label" + (picked.has(item.name) ? " picked" : "") + (item.gone ? " gone" : "")}>
        <input type="checkbox" checked={picked.has(item.name)} onChange={() => onToggle(item.name)} />
        <span>
          <span className="nm">{item.name}</span>
          {item.description && <small>{item.description}</small>}
          {showOwner && !item.gone && <small className="tag">{item.builtIn ? "Built in" : "Yours"}</small>}
        </span>
      </label>)}
    </div>
  </fieldset>;
}

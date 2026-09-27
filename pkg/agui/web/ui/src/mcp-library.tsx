import { useEffect, useRef, useState } from "react";
import { deleteMCPServer, fetchMCPServers, isGlobalMCPServer, mcpCallbackUrl, mcpConnectUrl, saveMCPServer, type MCPServerConfig, type MCPServerInfo } from "./api";

type Header = { key: string; value: string };

const emptyForm = {
  name: "", endpoint: "", transport: "streamable-http" as "streamable-http" | "sse", toolPrefix: "",
  oauth: false, clientId: "", clientSecret: "", authUrl: "", tokenUrl: "", scopes: "",
};

// buildConfig turns the form into the PUT body, leaving out empty optional fields.
function buildConfig(form: typeof emptyForm, headers: Header[]): MCPServerConfig {
  const config: MCPServerConfig = { endpoint: form.endpoint.trim(), transport: form.transport };
  if (form.toolPrefix.trim()) config.toolPrefix = form.toolPrefix.trim();
  const filled = headers.filter(header => header.key.trim());
  if (filled.length) config.headers = Object.fromEntries(filled.map(header => [header.key.trim(), header.value]));
  if (form.oauth) {
    config.authorization = {
      clientId: form.clientId.trim(), authUrl: form.authUrl.trim(), tokenUrl: form.tokenUrl.trim(),
      redirectUrl: mcpCallbackUrl(form.name.trim()),
    };
    if (form.clientSecret) config.authorization.clientSecret = form.clientSecret;
    const scopes = form.scopes.split(/[\s,]+/).filter(Boolean);
    if (scopes.length) config.authorization.scopes = scopes;
  }
  return config;
}

function status(server: MCPServerInfo): string {
  const parts = [isGlobalMCPServer(server) ? "Built in · always on" : "Yours"];
  if (server.transport) parts.push(server.transport);
  if (server.oauth) parts.push(server.connected === undefined ? "OAuth" : server.connected ? "OAuth · connected" : "OAuth · not connected");
  return parts.join(" · ");
}

export function MCPLibrary({onClose, onChanged}: {onClose: () => void; onChanged: () => void}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [servers, setServers] = useState<MCPServerInfo[]>([]);
  const [form, setForm] = useState(emptyForm);
  const [headers, setHeaders] = useState<Header[]>([{ key: "", value: "" }]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [pendingDelete, setPendingDelete] = useState<string | null>(null);

  async function load() {
    setBusy(true); setError("");
    try { setServers(await fetchMCPServers()); }
    catch (err) { setError(String(err)); }
    finally { setBusy(false); }
  }
  useEffect(() => {
    dialog.current?.showModal();
    void load();
    // OAuth sign-in happens in another tab; refresh connection status on return.
    const refresh = () => { void load(); onChanged(); };
    window.addEventListener("focus", refresh);
    return () => window.removeEventListener("focus", refresh);
  }, []);

  const set = (field: keyof typeof emptyForm) => (event: { target: { value: string } }) => setForm(old => ({ ...old, [field]: event.target.value }));
  const name = form.name.trim();
  const reserved = servers.some(server => server.name === name && isGlobalMCPServer(server));
  const invalidName = name !== "" && /[/\\]/.test(name);
  const ready = name !== "" && !invalidName && !reserved && form.endpoint.trim() !== "" &&
    (!form.oauth || (form.clientId.trim() !== "" && form.authUrl.trim() !== "" && form.tokenUrl.trim() !== ""));

  async function save() {
    setBusy(true); setError(""); setNotice("");
    try {
      const replacing = servers.some(server => server.name === name);
      await saveMCPServer(name, buildConfig(form, headers));
      setNotice(form.oauth
        ? `Saved ${name}. Connect your account to load its tools.`
        : `${replacing ? "Replaced" : "Added"} ${name}. It is on for every agent; turn it off in the MCP servers menu.`);
      setForm(emptyForm); setHeaders([{ key: "", value: "" }]);
      onChanged(); await load();
    } catch (err) { setError(String(err)); }
    finally { setBusy(false); }
  }
  async function remove(serverName: string) {
    setBusy(true); setError(""); setNotice("");
    try {
      await deleteMCPServer(serverName);
      setPendingDelete(null);
      setNotice(`Removed ${serverName} and any account connected to it.`);
      onChanged(); await load();
    } catch (err) { setError(String(err)); }
    finally { setBusy(false); }
  }

  return <dialog ref={dialog} className="skill-library mcp-library" onCancel={onClose} aria-labelledby="mcp-library-title">
    <header><div><h2 id="mcp-library-title">MCP servers</h2><p>Connect remote tools for every agent. Built-in servers come from the app and are always on.</p></div><button onClick={onClose} aria-label="Close MCP servers">Close</button></header>
    <details className="skill-upload" open>
      <summary>Add a server</summary>
      <fieldset disabled={busy} className="routine-form">
        <label>Name<input value={form.name} onChange={set("name")} placeholder="github" aria-invalid={invalidName || reserved} />
          <small>{reserved ? "A built-in server already uses this name." : invalidName ? "Names cannot contain slashes." : "Saving an existing name of yours replaces it."}</small></label>
        <label>Server URL<input type="url" value={form.endpoint} onChange={set("endpoint")} placeholder="https://mcp.example.com/mcp" />
          <small>Must be a public HTTP(S) address.</small></label>
        <label>Transport<select value={form.transport} onChange={set("transport")}>
          <option value="streamable-http">Streamable HTTP</option>
          <option value="sse">Server-sent events (SSE)</option>
        </select></label>
        <label>Tool prefix (optional)<input value={form.toolPrefix} onChange={set("toolPrefix")} placeholder={name ? `${name}__` : "github__"} />
          <small>Keeps this server's tool names distinct from other servers'. Include the separator.</small></label>
        <div className="mcp-headers" role="group" aria-label="Request headers">
          <span>Headers (optional)</span>
          <small>Send an API key or other fixed values with every request.</small>
          {headers.map((header, index) => <div className="mcp-header-row" key={index}>
            <input aria-label={`Header ${index + 1} name`} value={header.key} placeholder="Authorization"
              onChange={event => setHeaders(old => old.map((item, i) => i === index ? { ...item, key: event.target.value } : item))} />
            <input aria-label={`Header ${index + 1} value`} type="password" autoComplete="off" value={header.value} placeholder="Bearer …"
              onChange={event => setHeaders(old => old.map((item, i) => i === index ? { ...item, value: event.target.value } : item))} />
            <button type="button" aria-label={`Remove header ${index + 1}`} onClick={() => setHeaders(old => old.length > 1 ? old.filter((_, i) => i !== index) : [{ key: "", value: "" }])}>Remove</button>
          </div>)}
          <button type="button" onClick={() => setHeaders(old => [...old, { key: "", value: "" }])}>Add header</button>
        </div>
        <label className="mcp-oauth-toggle"><input type="checkbox" checked={form.oauth} onChange={event => setForm(old => ({ ...old, oauth: event.target.checked }))} />Sign in with OAuth</label>
        {form.oauth && <>
          <label>Client ID<input value={form.clientId} onChange={set("clientId")} /></label>
          <label>Client secret (optional)<input type="password" autoComplete="off" value={form.clientSecret} onChange={set("clientSecret")} /></label>
          <label>Authorization URL<input type="url" value={form.authUrl} onChange={set("authUrl")} placeholder="https://auth.example.com/authorize" /></label>
          <label>Token URL<input type="url" value={form.tokenUrl} onChange={set("tokenUrl")} placeholder="https://auth.example.com/token" /></label>
          <label>Scopes (optional)<input value={form.scopes} onChange={set("scopes")} placeholder="read write" /></label>
          <label>Redirect URL<input readOnly value={name ? mcpCallbackUrl(name) : ""} placeholder="Enter a name first" />
            <small>Register this URL with the OAuth provider.</small></label>
        </>}
        <button disabled={busy || !ready} onClick={() => void save()}>{busy ? "Working…" : "Save server"}</button>
      </fieldset>
    </details>
    {error && <p role="alert" className="skill-library-error">{error}</p>}
    {notice && <p role="status" className="skill-library-notice">{notice}</p>}
    <section aria-label="MCP servers">
      <h3>Servers <span className="skill-count">{servers.length}</span></h3>
      {!servers.length && <p className="skill-empty">{busy ? "Loading servers…" : "No MCP servers yet. Add one to give agents its tools."}</p>}
      {servers.map(server => <article key={server.name}>
        <div className="skill-summary"><strong>{server.name}</strong>
          <small>{status(server)}</small>
          {server.error && <p className="skill-library-error">{server.error}</p>}
        </div>
        <div className="skill-row-actions">
          {pendingDelete === server.name ? <div className="skill-delete-confirm" role="group" aria-label={`Confirm removal of ${server.name}`}>
            <p>Remove this server and any account connected to it?</p>
            <button disabled={busy} onClick={() => setPendingDelete(null)}>Cancel</button>
            <button className="skill-danger" disabled={busy} onClick={() => void remove(server.name)}>Confirm remove</button>
          </div> : <>
            {server.connected !== undefined && <a className="mcp-connect" href={mcpConnectUrl(server.name)} target="_blank" rel="noopener">{server.connected ? "Reconnect" : "Connect"}</a>}
            {!isGlobalMCPServer(server) && !server.readOnly && <button className="skill-danger" disabled={busy} aria-label={`Remove ${server.name}`} onClick={() => setPendingDelete(server.name)}>Remove</button>}
          </>}
        </div>
      </article>)}
    </section>
  </dialog>;
}

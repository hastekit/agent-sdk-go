import { useEffect, useRef, useState } from "react";
import { deleteSkill, fetchSkills, fetchStoredSkills, skillFileUrl, uploadSkill, type StoredSkill } from "./api";

import { validateSkillUpload } from "./skill-upload";

export function SkillLibrary({agentName, onClose, onSaved}: {agentName: string; onClose: () => void; onSaved: () => void}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [skills, setSkills] = useState<StoredSkill[]>([]);
  const [cursor, setCursor] = useState("");
  const [files, setFiles] = useState<File[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [selected, setSelected] = useState<StoredSkill | null>(null);
  const [pendingDelete, setPendingDelete] = useState<string | null>(null);
  const [content, setContent] = useState("");
  const previewRequest = useRef(0);

  async function load(after = "") {
    setBusy(true); setError("");
    try {
      const page = await fetchStoredSkills(after);
      setSkills(old => after ? [...old, ...page.skills.filter(s => !old.some(o => o.name === s.name))] : page.skills);
      setCursor(page.nextCursor || "");
    } catch (err) { setError(String(err)); }
    finally { setBusy(false); }
  }
  useEffect(() => { dialog.current?.showModal(); void load(); }, []);
  async function save() {
    setBusy(true); setError(""); setNotice("");
    try {
      const catalog = await fetchSkills(agentName);
      await validateSkillUpload(files, catalog.filter(skill => skill.global).map(skill => skill.name));
      const saved = await uploadSkill(files);
      setFiles([]); setSelected(null); previewRequest.current++;
      setNotice(`Saved ${saved.name}. Enable it in the agent’s Skills menu if it is opt-in.`);
      onSaved(); await load();
    } catch (err) { setError(String(err)); }
    finally { setBusy(false); }
  }
  async function remove(name: string) {
    setBusy(true); setError(""); setNotice("");
    try {
      await deleteSkill(name);
      if (selected?.name === name) { setSelected(null); setContent(""); previewRequest.current++; }
      setPendingDelete(null);
      setNotice(`Deleted ${name}. It is no longer available from this library.`);
      onSaved(); await load();
    } catch (err) { setError(String(err)); }
    finally { setBusy(false); }
  }
  async function view(skill: StoredSkill) {
    const request = ++previewRequest.current;
    setSelected(skill); setContent("Loading…"); setError("");
    try {
      const r = await fetch(skillFileUrl(skill.name, "SKILL.md"));
      if (!r.ok) throw new Error(await r.text());
      const text = await r.text();
      if (request === previewRequest.current) setContent(text);
    } catch (err) { if (request === previewRequest.current) {setContent(""); setError(String(err));} }
  }
  return <dialog ref={dialog} className="skill-library" onCancel={onClose} aria-labelledby="skill-library-title">
    <header><div><h2 id="skill-library-title">Skill library</h2><p>Manage the skills available to your agents.</p></div><button onClick={onClose} aria-label="Close skill library">Close</button></header>
    <details className="skill-upload" open>
      <summary>Upload a skill</summary>
      <p>Choose a folder with SKILL.md and supporting files, or select individual files. SKILL.md must include a name and description in its frontmatter.</p>
      <label>Skill folder<input type="file" multiple ref={input => input?.setAttribute("webkitdirectory", "")} disabled={busy}
        onChange={e => {setFiles(Array.from(e.target.files || [])); e.target.value = "";}} /></label>
      <label>Skill files<input type="file" multiple disabled={busy} onChange={e => {setFiles(Array.from(e.target.files || [])); e.target.value = "";}} /></label>
      <small>{files.length ? `${files.length} files selected` : "Up to 100 files, 10 MiB total. Folder selection preserves nested paths."}</small>
      <p>Uploading replaces your existing skill with the same name. Names used by this agent’s global skills are reserved.</p>
      <button disabled={busy || !files.length} onClick={() => void save()}>{busy ? "Working…" : "Upload / replace skill"}</button>
    </details>
    {error && <p role="alert" className="skill-library-error">{error}</p>}
    {notice && <p role="status" className="skill-library-notice">{notice}</p>}
    <section aria-label="Stored skills">
      <h3>Stored skills <span className="skill-count">{skills.length}{cursor ? "+" : ""}</span></h3>
      {!skills.length && <p className="skill-empty">{busy ? "Loading skills…" : "Your library is empty. Upload a skill to get started."}</p>}
      {skills.map(skill => <article key={skill.name} className={selected?.name === skill.name ? "selected" : ""}>
        <div className="skill-summary"><strong>{skill.name}</strong><p>{skill.description}</p><small>{1 + (skill.resources?.length || 0)} files</small></div>
        <div className="skill-row-actions">
          {pendingDelete === skill.name ? <div className="skill-delete-confirm" role="group" aria-label={`Confirm deletion of ${skill.name}`}>
            <p>Delete this skill and all its files?</p>
            <button disabled={busy} onClick={() => setPendingDelete(null)}>Cancel</button>
            <button className="skill-danger" disabled={busy} onClick={() => void remove(skill.name)}>Confirm delete</button>
          </div> : <>
            <button disabled={busy} aria-label={`View ${skill.name}`} onClick={() => void view(skill)}>View</button>
            <button className="skill-danger" disabled={busy} aria-label={`Delete ${skill.name}`} onClick={() => setPendingDelete(skill.name)}>Delete</button>
          </>}
        </div>
      </article>)}
      {cursor && <button disabled={busy} onClick={() => void load(cursor)}>Load more skills</button>}
    </section>
    {selected && <section className="skill-preview" aria-label="Skill contents"><header><h3>{selected.name}</h3><button onClick={() => { setSelected(null); previewRequest.current++; }}>Close preview</button></header>
      <pre>{content}</pre>
      <ul>{["SKILL.md", ...(selected.resources || [])].map(file => <li key={file}><a href={skillFileUrl(selected.name, file)} download>{file}</a></li>)}</ul>
    </section>}
  </dialog>;
}

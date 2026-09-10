import { useRef, useState } from "react";
import { CopilotChatInput } from "@copilotkit/react-core/v2";
import type { InputContent } from "@ag-ui/core";
import { uploadAttachment, type UploadedAttachment } from "./api";

// Only uploaded references enter messages; File objects live in the browser
// until upload completes. An input slot is remounted when its thread changes.
export function AttachmentInput({ onSteer, attachmentsEnabled, ...props }: any) {
  const [files, setFiles] = useState<UploadedAttachment[]>([]);
  const [uploading, setUploading] = useState(false);
  const [sending, setSending] = useState(false);
  const [error, setError] = useState("");
  const picker = useRef<HTMLInputElement>(null);
  const draft = useRef(props.value); draft.current = props.value;
  const busy = uploading || sending;
  const hasText = (props.value ?? "").trim().length > 0;
  const submit = async (text: string) => {
    if (busy) return;
    if (!files.length && !props.isRunning) { props.onSubmitMessage?.(text); return; }
    setSending(true); setError("");
    draft.current = ""; props.onChange?.("");
    try {
      const parts: InputContent[] = files.map(f => ({
        type: f.mediaType.startsWith("image/") ? "image" : "document",
        source: { type: "url", value: f.url, mimeType: f.mediaType },
        metadata: { filename: f.filename },
      }));
      await onSteer(text, parts);
      setFiles([]);
    } catch (e) {
      setError(String(e));
      if (!draft.current?.trim()) props.onChange?.(text);
    }
    finally { setSending(false); }
  };
  return <>
    {attachmentsEnabled && <div className="attachment-composer">
      <input ref={picker} type="file" hidden multiple
        accept="image/png,image/jpeg,image/gif,image/webp,application/pdf"
        onChange={async e => {
          const selected = Array.from(e.target.files ?? []); e.target.value = "";
          setUploading(true); setError("");
          try {
            for (const file of selected) {
              const uploaded = await uploadAttachment(file);
              setFiles(old => [...old, uploaded]);
            }
          } catch (e) { setError(String(e)); }
          finally { setUploading(false); }
        }} />
      <button type="button" disabled={busy} onClick={() => picker.current?.click()}>Attach files</button>
      {uploading && <span role="status">Uploading…</span>}
      {files.map((file, i) => <div className="attachment-preview" key={`${file.file_id}-${i}`}>
        {file.mediaType.startsWith("image/") && <img src={file.url} alt={file.filename} />}
        <a href={file.url} target="_blank" rel="noreferrer">{file.filename}</a>
        <button type="button" disabled={busy} aria-label={`Remove ${file.filename}`}
          onClick={() => setFiles(old => old.filter((_, n) => n !== i))}>×</button>
      </div>)}
      {files.length > 0 && <button type="button" disabled={busy} onClick={() => void submit(props.value ?? "")}>Send attachments</button>}
    </div>}
    {error && <div role="alert" className="attachment-error">{error}</div>}
    <CopilotChatInput {...props}
      isRunning={props.isRunning && !hasText && !files.length}
      onSubmitMessage={busy ? undefined : (text: string) => void submit(text)} />
  </>;
}

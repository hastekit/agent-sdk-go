import { useRef, useState, type Ref } from "react";
import { CopilotChatInput, CopilotChatAttachmentQueue, useAttachments } from "@copilotkit/react-core/v2";
import { ComposerMenu } from "./composer-menu";
import type { InputContent } from "@ag-ui/core";
import { uploadAttachment } from "./api";

// CopilotKit owns selection, validation, upload state, paste, and drag/drop.
// Only store references enter messages; browser previews use the download URL.
// The input slot remounts on thread changes, isolating pending uploads.
export function AttachmentInput({ onSteer, attachmentsEnabled, sessionId, ...props }: any) {
  const [sending, setSending] = useState(false);
  const [error, setError] = useState("");
  const previews = useRef(new Map<string, string>());
  const sendingRef = useRef(false);
  const draft = useRef(props.value); draft.current = props.value;
  const queue = useAttachments({ config: {
    enabled: attachmentsEnabled && !sending,
    maxSize: 20 * 1024 * 1024,
    onUpload: async file => {
      setError("");
      const uploaded = await uploadAttachment(file, sessionId);
      previews.current.set(uploaded.file_id, uploaded.url);
      return { type: "url", value: uploaded.file_id, mimeType: uploaded.mediaType,
        metadata: { filename: uploaded.filename } };
    },
    onUploadFailed: failure => setError(failure.message),
  } });
  const uploading = queue.attachments.some(file => file.status === "uploading");
  const busy = uploading || sending;
  const hasText = (props.value ?? "").trim().length > 0;
  const submit = async (text: string) => {
    if (!text.trim() || uploading || sendingRef.current) return;
    if (!queue.attachments.length && !props.isRunning) { props.onSubmitMessage?.(text); return; }
    sendingRef.current = true; setSending(true); setError("");
    const submitted = queue.attachments.filter(file => file.status === "ready");
    draft.current = ""; props.onChange?.("");
    try {
      const parts: InputContent[] = submitted.map(file => ({
        type: file.source.mimeType?.startsWith("image/") ? "image" : "document",
        source: file.source,
        metadata: { ...file.metadata, filename: file.metadata?.filename ?? file.filename },
      }));
      await onSteer(text, parts);
      // Remove only this submission, preserving the queue if sending fails.
      for (const file of submitted) {
        queue.removeAttachment(file.id);
        previews.current.delete(file.source.value);
      }
    } catch (e) {
      setError(String(e));
      if (!draft.current?.trim()) props.onChange?.(text);
    } finally { sendingRef.current = false; setSending(false); }
  };
  const previewAttachments = queue.attachments.map(file => ({
    ...file,
    source: file.status === "ready" && previews.current.has(file.source.value)
      ? { ...file.source, value: previews.current.get(file.source.value)! } : file.source,
  }));
  return <div ref={queue.containerRef as Ref<HTMLDivElement>} className={"attachment-input" + (queue.dragOver ? " attachment-drag-over" : "")}
    onDragOver={attachmentsEnabled && !sending ? queue.handleDragOver : undefined}
    onDragLeave={queue.handleDragLeave}
    onDrop={attachmentsEnabled && !sending ? queue.handleDrop : undefined}>
    {attachmentsEnabled && <>
      <input ref={queue.fileInputRef as Ref<HTMLInputElement>} type="file" hidden multiple disabled={sending}
        onChange={event => {
          void queue.handleFileUpload(event);
          // Allow selecting the same file again after sending or removing it.
          event.currentTarget.value = "";
        }} />
      <CopilotChatAttachmentQueue attachments={previewAttachments}
        onRemoveAttachment={id => {
          if (sendingRef.current) return;
          const file = queue.attachments.find(file => file.id === id);
          if (file) previews.current.delete(file.source.value);
          queue.removeAttachment(id);
        }} />
      {uploading && <div role="status" className="attachment-status">Uploading…</div>}
      {queue.dragOver && <div role="status" className="attachment-status">Drop files to attach</div>}
    </>}
    {error && <div role="alert" className="attachment-error">{error}</div>}
    <CopilotChatInput {...props}
      onAddFile={attachmentsEnabled ? () => { if (!sendingRef.current) queue.fileInputRef.current?.click(); } : undefined}
      addMenuButton={ComposerMenu}
      isRunning={props.isRunning && !hasText}
      onSubmitMessage={busy ? undefined : (text: string) => void submit(text)} />
  </div>;
}

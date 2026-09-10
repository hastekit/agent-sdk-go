import {
  CopilotChatMessageView,
  CopilotChatUserMessage,
  type CopilotChatUserMessageProps,
} from "@copilotkit/react-core/v2";

function AttachmentUserMessage(props: CopilotChatUserMessageProps) {
  const content = props.message.content;
  if (typeof content === "string") return <CopilotChatUserMessage {...props} />;
  // CopilotKit renders document cards without a link. Owned documents use
  // the authorized download endpoint so the user can retrieve the original.
  const documents = content.filter(part => part.type === "document" &&
    part.source.type === "url" && part.source.value.startsWith("/attachments/"));
  if (!documents.length) return <CopilotChatUserMessage {...props} />;
  return <div>
    <CopilotChatUserMessage {...props} message={{ ...props.message,
      content: content.filter(part => !documents.includes(part)),
    }} />
    <div className="attachment-documents">
      {documents.map((part, i) => {
        if (part.type !== "document") return null;
        const metadata = part.metadata as { filename?: string } | undefined;
        return <a key={i} href={part.source.value} download rel="noreferrer">
          <strong>PDF</strong> {metadata?.filename || "Download document"}
        </a>;
      })}
    </div>
  </div>;
}

export function AttachmentMessageView(props: any) {
  return <CopilotChatMessageView {...props} userMessage={AttachmentUserMessage as any} />;
}

// When a message was sent, shown beside it.
//
// A stored message says so itself: the server sends metadata.createdAt on
// every user and assistant message it hydrates, null when the message was
// stored before times were kept. A message without the key arrived live, and
// is timed by when this page first saw it — the moment it was sent or began
// streaming in.
interface TimedMessage {
  id: string;
  metadata?: Record<string, unknown>;
}

const firstSeen = new Map<string, number>();

export function messageTime(message: TimedMessage): Date | null {
  const metadata = message.metadata;
  if (metadata && "createdAt" in metadata) {
    const value = metadata.createdAt;
    if (typeof value !== "string") return null;
    const at = new Date(value);
    return Number.isNaN(at.getTime()) ? null : at;
  }
  let seen = firstSeen.get(message.id);
  if (seen === undefined) {
    seen = Date.now();
    firstSeen.set(message.id, seen);
  }
  return new Date(seen);
}

const timeOnly = new Intl.DateTimeFormat(undefined, { hour: "numeric", minute: "2-digit" });
const dayAndTime = new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
const fullDate = new Intl.DateTimeFormat(undefined, { year: "numeric", month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
const exact = new Intl.DateTimeFormat(undefined, { dateStyle: "full", timeStyle: "medium" });

export function formatMessageTime(at: Date, now = new Date()): string {
  if (at.toDateString() === now.toDateString()) return timeOnly.format(at);
  if (at.getFullYear() === now.getFullYear()) return dayAndTime.format(at);
  return fullDate.format(at);
}

export function MessageTime({ message }: { message: TimedMessage }) {
  const at = messageTime(message);
  if (!at) return null;
  return (
    <time className="message-time" dateTime={at.toISOString()} title={exact.format(at)}>
      {formatMessageTime(at)}
    </time>
  );
}

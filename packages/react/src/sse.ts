import type { ChatEvent } from "./types.js";

// Invalid protocol data is fatal because retrying the same cursor would repeat it.
export class InvalidStreamError extends Error {}

// A broker frame pairs a parsed event with its optional replay cursor.
export interface EventFrame {
  id?: string;
  event: ChatEvent;
}

// Decode byte chunks without acknowledging incomplete frames or corrupting Unicode.
export class SSEDecoder {
  private readonly decoder = new TextDecoder();
  private pending = "";

  // Preserve partial UTF-8 characters and CRLF separators across network reads.
  *push(bytes: Uint8Array): Generator<EventFrame> {
    this.pending += this.decoder.decode(bytes, { stream: true });
    this.pending = this.pending.replace(/\r\n/g, "\n");

    // Extract only complete frames and leave the trailing partial frame untouched.
    let boundary: number;
    while ((boundary = this.pending.indexOf("\n\n")) >= 0) {
      const text = this.pending.slice(0, boundary);
      this.pending = this.pending.slice(boundary + 2);
      const frame = this.parse(text);
      if (frame) yield frame;
    }
  }

  // Parse the data and cursor fields while ignoring SSE comments and metadata.
  private parse(frame: string): EventFrame | undefined {
    const data: string[] = [];
    let id: string | undefined;
    for (const line of frame.split("\n")) {
      if (line.startsWith("data:")) data.push(line.slice(5).replace(/^ /, ""));
      if (line.startsWith("id:")) id = line.slice(3).replace(/^ /, "");
    }

    // Keepalive frames have no application data, and NUL-containing cursors are invalid.
    if (!data.length) return undefined;
    if (id?.includes("\0")) id = undefined;

    // Require an event discriminator while allowing application-defined event types.
    try {
      const event = JSON.parse(data.join("\n")) as ChatEvent;
      if (!event || typeof event.type !== "string")
        throw new Error("Missing event type");
      return { id, event };
    } catch (error) {
      throw new InvalidStreamError(`Invalid SSE event: ${error}`);
    }
  }
}

import type { BaseEvent } from "@ag-ui/core";
import { Observable } from "rxjs";

/**
 * Runs a tool this client provides and resolves with its result, or returns
 * `undefined` when the tool is not one of its own.
 */
export type ClientToolRunner = (name: string, args: unknown, toolCallId: string) => Promise<unknown> | undefined;

export interface ToolResult {
  toolCallId: string;
  content: string;
}

export interface ClientToolOptions {
  runTool?: ClientToolRunner;
  /**
   * Sends results as tool messages on the thread. Resolves with the run they
   * started, or null when a live run took them instead.
   */
  send: (results: ToolResult[]) => Promise<Observable<BaseEvent> | null>;
  /**
   * Run tools as their calls stream in, as for a run this client started
   * (the default). A run it joined instead answers, when it ends, only the
   * calls to its tools still unanswered: its calls may already have been run,
   * by this client before a reload or by another one.
   */
  eager?: boolean;
}

/** Tool results are text for the model; structured values travel as JSON. */
export function toolResultContent(value: unknown): string {
  if (typeof value === "string") return value;
  if (value === undefined) return "";
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

export function parseArgs(text: string): unknown {
  try {
    return text ? JSON.parse(text) : {};
  } catch {
    return {};
  }
}

/**
 * Runs a client tool for a call, or returns undefined when the tool is not one
 * of the client's own. A failing tool still answers, so the model can react
 * instead of the run hanging.
 */
export function runClientTool(
  runTool: ClientToolRunner,
  name: string,
  argsText: string,
  toolCallId: string,
): Promise<ToolResult> | undefined {
  const pending = runTool(name, parseArgs(argsText), toolCallId);
  if (pending === undefined) return undefined;
  return Promise.resolve(pending)
    .then(toolResultContent, (error) => `Error: ${error instanceof Error ? error.message : String(error)}`)
    .then((content) => ({ toolCallId, content }));
}

// A run and the runs that continue it share one record of their calls, so a
// result is run and sent once however the work is split across runs.
interface Session {
  names: Map<string, string>;
  args: Map<string, string>;
  results: Map<string, Promise<ToolResult>>;
  // What sending each result came to: the run it started, or null.
  sent: Map<string, Promise<Observable<BaseEvent> | null>>;
  answered: Set<string>;
  // Runs a result started that are already being shown.
  followed: Set<Observable<BaseEvent>>;
}

function newSession(): Session {
  return {
    names: new Map(),
    args: new Map(),
    results: new Map(),
    sent: new Map(),
    answered: new Set(),
    followed: new Set(),
  };
}

// Lets go of a run a result started that nobody is going to show: its
// response is closed, and the run goes on on the server regardless.
function release(run: Observable<BaseEvent>) {
  run.subscribe({ error: () => {} }).unsubscribe();
}

/**
 * Runs this client's tools and sends each result as a tool message on the
 * thread as soon as it is ready, whatever the run is doing by then:
 *
 * - A server waiting for the result takes it into the run in flight, which
 *   carries on and reports the result on this stream.
 * - A server that pauses instead finishes the run successfully with the calls
 *   unanswered. The results then resume it: the run that continues it is the
 *   one a result already started, if the run had ended by the time it was
 *   sent, or one started with the results now. Its events are appended to
 *   this run's.
 *
 * Either way subscribers see one run with the tool's result in it, so
 * CopilotKit does not run the tool a second time.
 */
export function withClientTools(
  events: Observable<BaseEvent>,
  options: ClientToolOptions,
  session: Session = newSession(),
): Observable<BaseEvent> {
  const runTool = options.runTool;
  if (!runTool) return events;
  const eager = options.eager ?? true;
  const owner = session.results.size === 0 && session.names.size === 0;

  return new Observable<BaseEvent>((subscriber) => {
    let holding = false;
    let continuation: { unsubscribe(): void } | undefined;

    // Runs the tool for a call once, reporting whether the call is this client's.
    const start = (toolCallId: string): boolean => {
      if (session.results.has(toolCallId)) return true;
      const name = session.names.get(toolCallId);
      if (!name) return false;
      const result = runClientTool(runTool, name, session.args.get(toolCallId) ?? "", toolCallId);
      if (!result) return false;
      session.results.set(toolCallId, result);
      session.sent.set(
        toolCallId,
        result.then((message) => options.send([message])).catch(() => null),
      );
      return true;
    };

    // Calls a successful run left unanswered. An AG-UI 1.0 outcome may name
    // them; otherwise they are the calls that got no result.
    const unanswered = (event: BaseEvent & Record<string, any>): string[] => {
      const outcome = event.outcome as { type?: string; pendingToolCallIds?: string[] } | undefined;
      if (outcome && outcome.type !== "success") return [];
      if (outcome?.pendingToolCallIds?.length) return outcome.pendingToolCallIds;
      return [...session.names.keys()].filter((id) => !session.answered.has(id));
    };

    // The run that continues this one: the first run a pending result started
    // that is not shown yet, or one started with the results now.
    const resumeWith = async (pending: string[]): Promise<Observable<BaseEvent> | null> => {
      const messages = await Promise.all(pending.map((id) => session.results.get(id)!));
      for (const started of await Promise.all(pending.map((id) => session.sent.get(id)))) {
        if (started && !session.followed.has(started)) {
          session.followed.add(started);
          return started;
        }
      }
      return options.send(messages);
    };

    const continueWith = (pending: string[], held: BaseEvent) => {
      holding = true;
      resumeWith(pending).then(
        (next) => {
          if (subscriber.closed) {
            if (next) release(next);
            return;
          }
          if (!next) {
            // A run already going took the results; this one is over.
            subscriber.next(held);
            subscriber.complete();
            return;
          }
          session.followed.add(next);
          // The continuation is this client's own run: its calls run eagerly.
          continuation = withClientTools(next, { ...options, eager: true }, session).subscribe({
            // It is the same run to subscribers.
            next: (event) => {
              if (event.type !== "RUN_STARTED") subscriber.next(event);
            },
            error: (error) => subscriber.error(error),
            complete: () => subscriber.complete(),
          });
        },
        (error) => subscriber.error(error),
      );
    };

    const source = events.subscribe({
      next(event) {
        const e = event as BaseEvent & Record<string, any>;
        switch (e.type) {
          case "TOOL_CALL_START":
            session.names.set(e.toolCallId, e.toolCallName);
            break;
          case "TOOL_CALL_ARGS":
            session.args.set(e.toolCallId, (session.args.get(e.toolCallId) ?? "") + (e.delta ?? ""));
            break;
          case "TOOL_CALL_END":
            if (eager) start(e.toolCallId);
            break;
          case "TOOL_CALL_RESULT":
            session.answered.add(e.toolCallId);
            break;
          case "RUN_FINISHED": {
            const pending = unanswered(e);
            // Continue only when every pending call is this client's; otherwise
            // the client (or a person) has something to do and the run ends here.
            if (pending.length && pending.every((id) => start(id))) {
              continueWith(pending, event);
              return; // This RUN_FINISHED is held: the run carries on.
            }
            break;
          }
        }
        subscriber.next(event);
      },
      error: (error) => subscriber.error(error),
      complete() {
        // A held run completes when its continuation does.
        if (!holding) subscriber.complete();
      },
    });

    return () => {
      source.unsubscribe();
      continuation?.unsubscribe();
      // Runs results started that nobody followed are let go.
      if (owner) {
        for (const started of session.sent.values()) {
          void started.then((run) => {
            if (run && !session.followed.has(run)) release(run);
          });
        }
      }
    };
  });
}

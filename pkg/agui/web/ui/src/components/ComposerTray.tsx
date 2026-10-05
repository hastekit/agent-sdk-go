import type { BackgroundTask, ContextUsage, HastekitInterrupt, InterruptDecision } from "@hastekit/copilotkit";
import { CopilotChatInput } from "@copilotkit/react-core/v2";
import { createContext, useContext, useState, type HTMLAttributes } from "react";
import { AttachmentInput } from "../attachment-input";
import { ContextMeter } from "./ContextMeter";
import { InterruptCard } from "./Interrupts";

// What the composer tray is showing: work still running, and a pause waiting
// on the user — and, below the composer, how full the context window is.
//
// Through context rather than props because the tray renders inside
// CopilotChat's input slot, and the slot is a component type — passing this
// down would give it a new identity on every change, remounting the composer
// and taking whatever the user had half-typed with it.
export interface Tray {
  tasks: BackgroundTask[];
  interrupt: TrayInterrupt | null;
  contextUsage: ContextUsage | null;
}

export interface TrayInterrupt {
  // Which pause this is, so the publisher for one can be torn down after the
  // next has already taken its place without clearing it.
  key: string;
  interrupts: HastekitInterrupt[];
  onSubmit: (decisions: InterruptDecision[]) => void;
}

export const TrayContext = createContext<Tray>({ tasks: [], interrupt: null, contextUsage: null });

// Keep the task/approval tray above the attachment-aware composer, and the
// context meter below it.
export function SteerableInput(props: any) {
  const tray = useContext(TrayContext);
  return <><ComposerTray tray={tray} /><AttachmentInput {...props} disclaimer={ComposerFooter} /></>;
}

// ComposerFooter fills the composer's disclaimer slot: the context meter
// directly under the box, then CopilotKit's own disclaimer. The slot is the
// only place inside the composer below the box; anything rendered after the
// composer lands under the disclaimer instead. Defined once, here, because the
// slot is a component type and a new one each render would remount it.
function ComposerFooter(props: HTMLAttributes<HTMLDivElement>) {
  const { contextUsage } = useContext(TrayContext);
  return (
    <>
      {contextUsage && <ContextMeter usage={contextUsage} />}
      <CopilotChatInput.Disclaimer {...props} className={contextUsage ? "composer-disclaimer" : undefined} />
    </>
  );
}

// ComposerTray is the one place the chat says what it is waiting on — a tool
// still working, a decision it needs — directly above the box the user would
// answer in.
//
// It renders inside the composer's slot because that is where the answer is
// given. It used to be two places: an approval that arrived live was drawn
// inline among the messages by CopilotKit, and the same approval after a
// reload was drawn above the transcript, so the same question moved depending
// on how the page came to know about it. It also puts the note within reach
// of a long conversation, where the top of the transcript is nowhere near
// where the user is reading.
//
// Sitting in CopilotChat's input overlay means the transcript's bottom padding
// tracks it — the overlay is measured — so nothing is left hidden behind it.
function ComposerTray({ tray }: { tray: Tray }) {
  if (!tray.tasks.length && !tray.interrupt) return null;

  return (
    <div className="composer-tray">
      {tray.tasks.length > 0 && (
        // Keyed by the tasks it is about, so dismissing it hides that set and
        // a job starting later says so rather than staying silent because the
        // note was waved away once.
        <BackgroundTaskNote
          key={tray.tasks.map((t) => t.taskId).join(",")}
          tasks={tray.tasks}
        />
      )}
      {tray.interrupt && (
        <InterruptCard
          key={tray.interrupt.key}
          interrupts={tray.interrupt.interrupts}
          onSubmit={tray.interrupt.onSubmit}
        />
      )}
    </div>
  );
}

// BackgroundTaskNote says a tool is still working after the turn that called
// it finished.
//
// Without it a reloaded page reads as though the agent simply stopped
// talking: the tool answered, the run ended, and the actual work is somewhere
// else entirely. The note is dismissible because the task may well outlast
// the user's interest in being told about it.
function BackgroundTaskNote({ tasks }: { tasks: BackgroundTask[] }) {
  const [hidden, setHidden] = useState(false);
  if (hidden) return null;

  return (
    <div className="background-note" role="status">
      <span className="spinner" aria-hidden="true" />
      <div className="msg">
        {tasks.length === 1
          ? `${tasks[0].toolName || "A background job"} is still running. Its result will arrive here when it finishes.`
          : `${tasks.length} background jobs are still running. Their results will arrive here when they finish.`}
      </div>
      <button
        className="dismiss"
        onClick={() => setHidden(true)}
        aria-label="Dismiss"
      >
        ×
      </button>
    </div>
  );
}

import {
  createContext,
  createElement,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useSyncExternalStore,
} from "react";
import type { ReactNode } from "react";
import { ChatController } from "./controller.js";
import type { ChatOptions } from "./types.js";

// The context shares one store between sidebar, transcript, and composer components.
const ChatContext = createContext<ChatController | null>(null);

// Subscribe to the immutable controller snapshot and expose stable action functions.
function useController(controller: ChatController) {
  const snapshot = useSyncExternalStore(
    controller.subscribe,
    controller.getSnapshot,
    controller.getSnapshot,
  );
  return {
    ...snapshot,
    refreshSkills: controller.refreshSkills,
    setSkillEnabled: controller.setSkillEnabled,
    resetSkills: controller.resetSkills,
    newThread: controller.newThread,
    selectThread: controller.selectThread,
    refreshThreads: controller.refreshThreads,
    loadOlderMessages: controller.loadOlderMessages,
    sendMessage: controller.sendMessage,
    stop: controller.stop,
    resume: controller.resume,
    reconnect: controller.reconnect,
    disconnect: controller.disconnect,
    clearError: controller.clearError,
    uploadAttachment: controller.uploadAttachment,
  };
}

// Keep callbacks current without replacing a running controller on ordinary rerenders.
function useManagedController(options: ChatOptions): ChatController {
  const latest = useRef(options);
  latest.current = options;
  const controller = useMemo(
    () =>
      new ChatController({
        ...options,
        get forwardedProps() {
          return latest.current.forwardedProps;
        },
        get context() {
          return latest.current.context;
        },
        onEvent: (event) => latest.current.onEvent?.(event),
        createId: () =>
          latest.current.createId?.() ?? globalThis.crypto.randomUUID(),
      }),
    [
      options.agent,
      options.transport,
      options.groupId,
      options.initialThreadId,
      options.fullHistory,
      options.watchRuns,
    ],
  );

  // StrictMode cleanup detaches requests, and the next setup restores the selected thread.
  useEffect(() => controller.mount(), [controller]);
  return controller;
}

// Own a controller in one component using a stable transport instance.
export function useChat(options: ChatOptions) {
  const controller = useManagedController(options);
  return useController(controller);
}

// Share the controller when chat controls live in separate React subtrees.
export function ChatProvider({
  options,
  children,
}: {
  options: ChatOptions;
  children: ReactNode;
}) {
  const controller = useManagedController(options);
  return createElement(ChatContext.Provider, { value: controller }, children);
}

// Read the provider-owned store without opening another stream or run feed.
export function useChatContext() {
  const controller = useContext(ChatContext);
  if (!controller)
    throw new Error("useChatContext must be used inside ChatProvider");
  return useController(controller);
}

// Export the hook result for reusable application components.
export type UseChatResult = ReturnType<typeof useChat>;

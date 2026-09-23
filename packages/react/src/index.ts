// Export the headless React API and the framework-independent client primitives.
export { useChat, ChatProvider, useChatContext } from "./react.js";
export type { UseChatResult } from "./react.js";
export { ChatController } from "./controller.js";
export { createAGUITransport } from "./transport.js";
export type { AGUITransportOptions } from "./transport.js";
export {
  HTTPError,
  StreamUnavailableError,
  InvalidStreamError,
} from "./stream.js";
export type * from "./types.js";

// Export routine management without coupling it to a conversation provider.
export { useRoutines, useRoutineThreads } from "./routines.js";
export type {
  Routine,
  RoutineDefinition,
  RoutineSchedule,
  RoutineRun,
  RoutineStatus,
  RoutineTransport,
} from "./routines.js";

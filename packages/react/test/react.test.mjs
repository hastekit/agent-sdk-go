import test from "node:test";
import assert from "node:assert/strict";
import React from "react";
import TestRenderer, { act } from "react-test-renderer";
import { ChatProvider, useChat, useChatContext } from "../dist/index.js";
import { transport } from "./helpers.mjs";

// Enable React's async act checks for both supported major versions.
globalThis.IS_REACT_ACT_ENVIRONMENT = true;

// Inline option objects must not reset a controller during ordinary React updates.
test("useChat preserves draft state across rerenders", async () => {
  const client = transport();
  let chat;
  function App({ label }) {
    chat = useChat({ agent: "a", transport: client });
    return React.createElement("div", null, label);
  }
  let root;
  await act(async () => {
    root = TestRenderer.create(React.createElement(App, { label: "one" }));
  });
  await act(async () => {
    chat.newThread();
  });
  const thread = chat.threadId;
  await act(async () => {
    root.update(React.createElement(App, { label: "two" }));
  });
  assert.equal(chat.threadId, thread);
  await act(async () => {
    root.unmount();
  });
});

// Separate UI components must share exactly one controller through the provider.
test("context shares selection between sidebar and transcript", async () => {
  const client = transport();
  let sidebar, transcript;
  function Sidebar() {
    sidebar = useChatContext();
    return null;
  }
  function Transcript() {
    transcript = useChatContext();
    return null;
  }
  let root;
  await act(async () => {
    root = TestRenderer.create(
      React.createElement(
        ChatProvider,
        { options: { agent: "a", transport: client } },
        React.createElement(Sidebar),
        React.createElement(Transcript),
      ),
    );
  });
  await act(async () => {
    sidebar.newThread();
  });
  assert.equal(sidebar.threadId, transcript.threadId);
  await act(async () => {
    root.unmount();
  });
});

// Applications reach steering and its queue through the hook, like every other action.
test("useChat exposes steering", async () => {
  let posted;
  const client = transport({
    stream: async function* (_agent, _thread, input) {
      if (input) yield { type: "RUN_STARTED", runId: "run" };
      await new Promise(() => {});
    },
    steer: async (_agent, _thread, input) => {
      posted = input;
      return true;
    },
  });
  let chat;
  function App() {
    chat = useChat({ agent: "a", transport: client });
    return null;
  }
  let root;
  await act(async () => {
    root = TestRenderer.create(React.createElement(App));
  });
  await act(async () => {
    void chat.sendMessage("first");
    await new Promise((resolve) => setTimeout(resolve, 5));
  });
  await act(async () => {
    await chat.steer("second");
  });
  assert.equal(posted.messages[0].content, "second");
  assert.deepEqual(chat.queuedMessageIds, [posted.messages[0].id]);
  await act(async () => {
    root.unmount();
  });
});

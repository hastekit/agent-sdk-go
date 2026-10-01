import type { Message } from "@ag-ui/core";
import { describe, expect, it } from "vitest";
import { HastekitAgent, newTurnOf } from "../src/agent";
import { fetchThreadMessages, watchRuns } from "../src/client";
import { HastekitEvent } from "../src/types";

interface Request {
  url: string;
  method: string;
  headers: Headers;
  body?: any;
}

// A fake HasteKit server: every run and stream request answers with one run's events.
function server(runEvents: (runId: string) => object[] = defaultRun) {
  const requests: Request[] = [];
  const fetch = async (url: string, init: RequestInit) => {
    const body = typeof init.body === "string" ? JSON.parse(init.body) : undefined;
    requests.push({ url, method: init.method ?? "GET", headers: new Headers(init.headers), body });
    if (url.endsWith("/stop")) return new Response(null, { status: 204 });
    const events = runEvents(body?.runId ?? "joined");
    return new Response(events.map((e, i) => `id: ${i + 1}\ndata: ${JSON.stringify(e)}\n\n`).join(""), {
      headers: { "Content-Type": "text/event-stream" },
    });
  };
  return { fetch, requests };
}

function defaultRun(runId: string) {
  return [
    { type: "RUN_STARTED", threadId: "thread", runId },
    { type: "CUSTOM", name: HastekitEvent.StreamId, value: { streamId: "stream-1" } },
    { type: "TEXT_MESSAGE_START", messageId: `reply-${runId}`, role: "assistant" },
    { type: "TEXT_MESSAGE_CONTENT", messageId: `reply-${runId}`, delta: "hello" },
    { type: "TEXT_MESSAGE_END", messageId: `reply-${runId}` },
    { type: "RUN_FINISHED", threadId: "thread", runId },
  ];
}

const user = (id: string, content = id): Message => ({ id, role: "user", content });
const assistant = (id: string): Message => ({ id, role: "assistant", content: id });

describe("HastekitAgent", () => {
  it("posts only the new turn, with the user's selections, to the agent's run endpoint", async () => {
    const { fetch, requests } = server();
    const agent = new HastekitAgent({
      agentName: "Support Bot",
      baseUrl: "https://api.example.test/agui/",
      threadId: "thread",
      headers: { Authorization: "Bearer t" },
      initialMessages: [user("old"), assistant("answer")],
      skillSelection: { disable: ["writing"] },
      mcpSelection: { disable: ["notes"] },
      fetch,
    });
    agent.addMessage(user("new"));
    await agent.runAgent({ forwardedProps: { tenant: "acme" } });

    const [run] = requests;
    expect(run.url).toBe("https://api.example.test/agui/agents/Support%20Bot/run");
    expect(run.headers.get("Authorization")).toBe("Bearer t");
    expect(run.body.messages.map((m: Message) => m.id)).toEqual(["new"]);
    expect(run.body.forwardedProps).toEqual({ tenant: "acme", skills: { disable: ["writing"] }, mcp: { disable: ["notes"] } });
    expect(agent.agentId).toBe("Support Bot");
  });

  it("posts the whole conversation in full-history mode", async () => {
    const { fetch, requests } = server();
    const agent = new HastekitAgent({ agentName: "a", threadId: "thread", fullHistory: true, fetch, initialMessages: [user("u1"), assistant("a1")] });
    agent.addMessage(user("u2"));
    await agent.runAgent();
    expect(requests[0].body.messages.map((m: Message) => m.id)).toEqual(["u1", "a1", "u2"]);
  });

  it.each(["none", "text", "custom"])("does not resend a failed turn with the next message (%s echo)", async (echo) => {
    const { fetch, requests } = server((runId) => runId === "failed" ? [
      { type: "RUN_STARTED", threadId: "thread", runId },
      ...(echo === "text" ? [
        { type: "TEXT_MESSAGE_START", messageId: "msg_u1", role: "user" },
        { type: "TEXT_MESSAGE_CONTENT", messageId: "msg_u1", delta: "hey" },
        { type: "TEXT_MESSAGE_END", messageId: "msg_u1" },
      ] : echo === "custom" ? [
        { type: "CUSTOM", name: HastekitEvent.InputMessage, value: user("msg_u1", "hey") },
      ] : []),
      { type: "RUN_ERROR", message: "Provider failed" },
    ] : defaultRun(runId));
    const agent = new HastekitAgent({ agentName: "a", threadId: "thread", fetch });
    agent.addMessage(user("u1", "hey"));
    await agent.runAgent({ runId: "failed" });
    agent.addMessage(user("u2", "list my previous messages"));
    await agent.runAgent({ runId: "next" });

    expect(requests.map((r) => r.body.messages)).toEqual([
      [user("u1", "hey")],
      [user("u2", "list my previous messages")],
    ]);
    expect(agent.messages.filter((m) => m.role === "user")).toHaveLength(2);
  });

  it("excludes failed messages loaded from history but sends every newly added message", async () => {
    const { fetch, requests } = server();
    const agent = new HastekitAgent({ agentName: "a", threadId: "thread", fetch, initialMessages: [user("failed", "hey")] });
    agent.prependMessages([user("older", "hey")]);
    agent.addMessage(user("new-1", "hey"));
    agent.addMessage(user("new-2", "hey"));
    await agent.runAgent();
    expect(requests[0].body.messages).toEqual([user("new-1", "hey"), user("new-2", "hey")]);
  });

  it.each(["text", "custom"])("excludes a failed turn received while joining another client's run (%s echo)", async (echo) => {
    const { fetch, requests } = server((runId) => runId === "joined" ? [
      { type: "RUN_STARTED", threadId: "thread", runId },
      ...(echo === "text" ? [
        { type: "TEXT_MESSAGE_START", messageId: "msg_remote", role: "user" },
        { type: "TEXT_MESSAGE_CONTENT", messageId: "msg_remote", delta: "hey" },
        { type: "TEXT_MESSAGE_END", messageId: "msg_remote" },
      ] : [
        { type: "CUSTOM", name: HastekitEvent.InputMessage, value: user("msg_remote", "hey") },
      ]),
      { type: "RUN_ERROR", message: "Provider failed" },
    ] : defaultRun(runId));
    const agent = new HastekitAgent({ agentName: "a", threadId: "thread", fetch });
    await agent.connectAgent();
    expect(agent.messages).toEqual([user("msg_remote", "hey")]);
    agent.addMessage(user("new"));
    await agent.runAgent();
    expect(requests[1].body.messages).toEqual([user("new")]);
  });

  it("still sends failed turns in full-history mode for stateless servers", async () => {
    const { fetch, requests } = server(() => [{ type: "RUN_ERROR", message: "Provider failed" }]);
    const agent = new HastekitAgent({ agentName: "a", threadId: "thread", fullHistory: true, fetch });
    agent.addMessage(user("failed"));
    await agent.runAgent();
    agent.addMessage(user("new"));
    await agent.runAgent();
    expect(requests[1].body.messages).toEqual([user("failed"), user("new")]);
  });

  it("keeps submitted turns across clones without sharing their bookkeeping", async () => {
    const { fetch, requests } = server(() => [{ type: "RUN_ERROR", message: "Provider failed" }]);
    const agent = new HastekitAgent({ agentName: "a", threadId: "thread", fetch });
    agent.addMessage(user("failed"));
    await agent.runAgent();
    const copy = agent.clone();
    copy.addMessage(user("new"));
    await copy.runAgent();
    agent.addMessage(user("new"));
    await agent.runAgent();
    expect(requests.map((r) => r.body.messages)).toEqual([[user("failed")], [user("new")], [user("new")]]);
  });

  it("does not resend a steered message when the user starts the next turn", async () => {
    const { fetch, requests } = server();
    const agent = new HastekitAgent({ agentName: "a", threadId: "thread", fetch: async (url, init) => {
      if (JSON.parse(String(init.body)).messages[0]?.content === "steered") return new Response(null, { status: 204 });
      return fetch(url, init);
    } });
    await agent.steer("steered");
    agent.addMessage(user("new"));
    await agent.runAgent();
    expect(requests[0].body.messages).toEqual([user("new")]);
  });

  it("keeps loaded history when the agent is cleared to join the thread", async () => {
    const { fetch, requests } = server();
    const agent = new HastekitAgent({ agentName: "a", threadId: "thread", fetch, initialMessages: [user("u1"), assistant("a1")] });
    // CopilotKit's connect clears the agent before attaching to the thread.
    agent.setMessages([]);
    await agent.connectAgent();
    expect(requests[0]).toMatchObject({ url: "/api/agui/agents/a/threads/thread/stream", method: "GET" });
    expect(agent.messages.map((m) => m.id)).toEqual(["u1", "a1", "reply-joined"]);
  });

  it("orders older pages ahead of the conversation, even during a run", async () => {
    const { fetch } = server();
    const agent = new HastekitAgent({ agentName: "a", threadId: "thread", fetch, initialMessages: [user("recent")] });
    agent.subscribe({
      onRunStartedEvent: () => {
        agent.prependMessages([user("older"), user("recent")]);
        agent.prependMessages([user("older")]);
      },
    });
    await agent.runAgent({ runId: "r1" });
    expect(agent.messages.map((m) => m.id)).toEqual(["older", "recent", "reply-r1"]);
  });

  it("stops a run on the server once the server has named its stream", async () => {
    const { fetch, requests } = server();
    const agent = new HastekitAgent({ agentName: "a", threadId: "thread", fetch });
    let stopped = false;
    agent.subscribe({
      onCustomEvent: ({ event }) => {
        if (event.name === HastekitEvent.StreamId && !stopped) {
          stopped = true;
          queueMicrotask(() => agent.abortRun());
        }
      },
    });
    await agent.runAgent();
    await new Promise((resolve) => setTimeout(resolve, 0));
    const stop = requests.find((r) => r.url.endsWith("/stop"));
    expect(stop).toMatchObject({ method: "POST", body: { threadId: "thread", streamId: "stream-1" } });
  });

  it("steers a run in flight with only the new message, and withdraws it if it cannot be sent", async () => {
    const requests: Request[] = [];
    let status = 204;
    const fetch = async (url: string, init: RequestInit) => {
      requests.push({ url, method: init.method ?? "GET", headers: new Headers(init.headers), body: JSON.parse(String(init.body)) });
      return new Response(null, { status });
    };
    const agent = new HastekitAgent({ agentName: "a", threadId: "thread", fetch, initialMessages: [user("u1"), assistant("a1")] });

    await agent.steer("also check the logs");
    expect(requests[0].url).toBe("/api/agui/agents/a/run");
    expect(requests[0].body.messages).toEqual([expect.objectContaining({ role: "user", content: "also check the logs" })]);
    expect(agent.messages).toHaveLength(3);

    status = 409;
    await expect(agent.steer("again")).rejects.toThrow(/409/);
    expect(agent.messages).toHaveLength(3);
  });

  it("answers restored interrupts with resume entries and joins the resulting run", async () => {
    const { fetch, requests } = server();
    const agent = new HastekitAgent({ agentName: "a", threadId: "thread", fetch, initialMessages: [user("u1"), assistant("a1")] });
    const entries = [{ interruptId: "call_1", status: "resolved" as const, payload: { approved: true } }];
    await agent.resume(entries);
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(requests[0].body).toMatchObject({ messages: [], resume: entries });
    expect(requests[1]).toMatchObject({ url: "/api/agui/agents/a/threads/thread/stream", method: "GET" });
  });

  it("answers a client tool with a tool message that resumes the paused run", async () => {
    const bodies: any[] = [];
    const fetch = async (_url: string, init: RequestInit) => {
      const body = JSON.parse(String(init.body));
      bodies.push(body);
      const runId = body.runId;
      const events =
        bodies.length === 1
          ? [
              { type: "RUN_STARTED", threadId: "thread", runId },
              { type: "TOOL_CALL_START", toolCallId: "c1", toolCallName: "get_selection" },
              { type: "TOOL_CALL_ARGS", toolCallId: "c1", delta: "{}" },
              { type: "TOOL_CALL_END", toolCallId: "c1" },
              // Paused on the client's tool: a success with the call unanswered.
              { type: "RUN_FINISHED", threadId: "thread", runId, outcome: { type: "success" } },
            ]
          : [
              { type: "RUN_STARTED", threadId: "thread", runId },
              { type: "TOOL_CALL_RESULT", messageId: "r1", toolCallId: "c1", content: body.messages[0].content },
              { type: "RUN_FINISHED", threadId: "thread", runId, outcome: { type: "success" } },
            ];
      return new Response(events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join(""), {
        headers: { "Content-Type": "text/event-stream" },
      });
    };
    const agent = new HastekitAgent({ agentName: "a", threadId: "thread", fetch });
    agent.clientToolRunner = (name) => (name === "get_selection" ? Promise.resolve("selected text") : undefined);
    agent.addMessage(user("u1"));
    await agent.runAgent({ resume: [{ interruptId: "old", status: "resolved" }] });

    expect(bodies).toHaveLength(2);
    expect(bodies[1].messages).toEqual([expect.objectContaining({ role: "tool", toolCallId: "c1", content: "selected text" })]);
    expect(bodies[1].resume).toBeUndefined();
    expect(agent.messages.find((m) => m.role === "tool")).toMatchObject({ toolCallId: "c1", content: "selected text" });
  });

  it("sends the last run's tools and context with the requests it makes itself", async () => {
    const { fetch, requests } = server();
    const agent = new HastekitAgent({ agentName: "a", threadId: "thread", fetch });
    const tools = [{ name: "get_selection", description: "Read the selection", parameters: { type: "object", properties: {} } }];
    const context = [{ description: "page", value: "/reports" }];
    agent.addMessage(user("u1"));
    await agent.runAgent({ tools, context });

    await agent.resume([{ interruptId: "call_1", status: "resolved", payload: { approved: true } }]);
    const resume = requests.find((r) => r.body?.resume);
    expect(resume?.body).toMatchObject({ tools, context });
  });

  it("answers client tool calls a paused thread is waiting on, once", async () => {
    const { fetch, requests } = server();
    const call: Message = {
      id: "a1",
      role: "assistant",
      toolCalls: [{ id: "c1", type: "function", function: { name: "get_selection", arguments: '{"max":2}' } }],
    };
    const agent = new HastekitAgent({ agentName: "a", threadId: "thread", fetch, initialMessages: [user("u1"), call] });
    const ran: unknown[] = [];
    agent.clientToolRunner = (name, args) => {
      if (name !== "get_selection") return undefined;
      ran.push(args);
      return Promise.resolve("selected");
    };

    await agent.answerClientTools(["c1"]);
    await agent.answerClientTools(["c1"]);
    expect(ran).toEqual([{ max: 2 }]);
    const posts = requests.filter((r) => r.method === "POST" && r.url.endsWith("/run"));
    expect(posts).toHaveLength(1);
    expect(posts[0].body.messages).toEqual([expect.objectContaining({ role: "tool", toolCallId: "c1", content: "selected" })]);
  });

  it("sends the client's current tools with results answered before any run", async () => {
    const { fetch, requests } = server();
    const call: Message = {
      id: "a1",
      role: "assistant",
      toolCalls: [{ id: "c1", type: "function", function: { name: "get_selection", arguments: "{}" } }],
    };
    const agent = new HastekitAgent({ agentName: "a", threadId: "thread", fetch, initialMessages: [user("u1"), call] });
    const tools = [{ name: "get_selection", description: "Read the selection", parameters: { type: "object", properties: {} } }];
    agent.clientToolRunner = () => Promise.resolve("selected");
    // A page that just loaded has run nothing; the tools come from CopilotKit.
    agent.clientTools = () => tools;
    await agent.answerClientTools(["c1"]);
    const post = requests.find((r) => r.method === "POST" && r.url.endsWith("/run"));
    expect(post?.body.tools).toEqual(tools);
  });

  it("keeps the tools a connect was given for the requests it makes itself", async () => {
    const { fetch, requests } = server();
    const agent = new HastekitAgent({ agentName: "a", threadId: "thread", fetch });
    const tools = [{ name: "get_selection", description: "Read the selection", parameters: { type: "object", properties: {} } }];
    await agent.connectAgent({ tools });
    await agent.resume([{ interruptId: "call_1", status: "resolved", payload: { approved: true } }]);
    expect(requests.find((r) => r.body?.resume)?.body.tools).toEqual(tools);
  });

  it("clones into an independent HastekitAgent with the same configuration", async () => {
    const { fetch, requests } = server();
    const agent = new HastekitAgent({ agentName: "a", baseUrl: "/x", threadId: "thread", fetch, initialMessages: [user("u1")] });
    agent.skillSelection = { disable: ["s"] };
    const copy = agent.clone();
    expect(copy).toBeInstanceOf(HastekitAgent);
    expect(copy.agentName).toBe("a");
    expect(copy.connection.baseUrl).toBe("/x");

    copy.setMessages([]);
    await copy.connectAgent();
    // The copy restored its own history and updated only itself.
    expect(copy.messages.map((m) => m.id)).toEqual(["u1", "reply-joined"]);
    expect(agent.messages.map((m) => m.id)).toEqual(["u1"]);
    copy.addMessage(user("u2"));
    await copy.runAgent();
    expect(requests.at(-1)!.body.forwardedProps.skills).toEqual({ disable: ["s"] });
  });
});

describe("newTurnOf", () => {
  it("takes the trailing client messages, or trailing tool results", () => {
    expect(newTurnOf([user("u1"), assistant("a1"), user("u2"), user("u3")]).map((m) => m.id)).toEqual(["u2", "u3"]);
    expect(newTurnOf([user("u1")]).map((m) => m.id)).toEqual(["u1"]);
    expect(newTurnOf([user("u1"), assistant("a1")])).toEqual([]);
    const tool: Message = { id: "t1", role: "tool", toolCallId: "c1", content: "ok" };
    expect(newTurnOf([user("u1"), assistant("a1"), tool]).map((m) => m.id)).toEqual(["t1"]);
  });
});

describe("REST helpers", () => {
  it("loads a history page with its cursor and run state", async () => {
    const urls: string[] = [];
    const fetch = async (url: string) => {
      urls.push(url);
      return Response.json({
        messages: [user("u1")],
        run: { awaitingApproval: true },
        context: { tokens: 1234 },
        nextCursor: "older",
      });
    };
    const page = await fetchThreadMessages({ agentName: "a", fetch }, "thread", { cursor: "opaque+cursor", limit: 20 });
    const query = new URL(urls[0], "http://local").searchParams;
    expect(query.get("limit")).toBe("20");
    expect(query.get("cursor")).toBe("opaque+cursor");
    expect(page).toMatchObject({
      nextCursor: "older",
      sessionId: "thread",
      run: { awaitingApproval: true },
      context: { tokens: 1234 },
    });
  });

  it("reports an unsupported run feed instead of failing", async () => {
    const fetch = async () => new Response("", { status: 501 });
    await expect(watchRuns({ agentName: "a", fetch }, { cursor: "c" })).resolves.toEqual({ events: [], cursor: "c", supported: false });
  });
});

"""Real TCP interoperability tests: Google ADK / Python A2A SDK -> Hastekit."""

import asyncio
import json
import os
from pathlib import Path
import select
import signal
import socket
import subprocess
import tempfile
import unittest
import uuid

import httpx
from a2a.client import A2ACardResolver, ClientConfig, ClientFactory
from a2a.types import GetTaskRequest, Message, Part, Role, SendMessageRequest, TaskState
from google.adk.runners import InMemoryRunner
from google.genai import types
from google.protobuf.json_format import MessageToDict, ParseDict
import uvicorn

from adk_agent import APP_NAME, create_a2a_app, create_agent, event_values


EXAMPLE = Path(__file__).resolve().parent
REPO = EXAMPLE.parents[2]


def request(text, *, context_id=""):
    return SendMessageRequest(message=Message(
        message_id=str(uuid.uuid4()), context_id=context_id,
        role=Role.ROLE_USER, parts=[Part(text=text)],
    ))


def report(prompt):
    return {"agent": "hastekit", "request": prompt, "status": "ok", "items": ["text replies", "JSON artifacts"]}


class InteropTests(unittest.IsolatedAsyncioTestCase):
    @classmethod
    def setUpClass(cls):
        cls.base_url = os.environ.get("HASTEKIT_A2A_URL")
        if cls.base_url:
            return
        temporary = tempfile.TemporaryDirectory(prefix="hastekit-adk-")
        cls.addClassCleanup(temporary.cleanup)
        binary = Path(temporary.name) / "server"
        subprocess.run(["go", "build", "-o", str(binary), "./examples/agents/16_a2a_adk"],
                       cwd=REPO, check=True, timeout=180)
        log = tempfile.TemporaryFile(mode="w+")
        cls.addClassCleanup(log.close)
        process = subprocess.Popen([str(binary), "-addr", "127.0.0.1:0"],
                                   stdout=subprocess.PIPE, stderr=log, text=True)

        def stop():
            if process.poll() is None:
                process.send_signal(signal.SIGINT)
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
            process.stdout.close()

        cls.addClassCleanup(stop)
        if not select.select([process.stdout], [], [], 15)[0]:
            raise RuntimeError("Hastekit server did not start within 15 seconds")
        line = process.stdout.readline().strip()
        if not line.startswith("LISTEN "):
            log.seek(0)
            raise RuntimeError(f"Hastekit startup failed: {line}\n{log.read()}")
        cls.base_url = line.removeprefix("LISTEN ")

    async def asyncSetUp(self):
        self.methods = []

        async def record(req):
            if req.method == "POST":
                self.methods.append(json.loads(req.content)["method"])

        self.http = httpx.AsyncClient(timeout=15, trust_env=False, event_hooks={"request": [record]})
        self.addAsyncCleanup(self.http.aclose)

    async def client(self, name="Echo", streaming=False):
        endpoint = f"{self.base_url}/api/agui/a2a/{name}"
        card = await A2ACardResolver(self.http, endpoint).get_agent_card()
        return ClientFactory(ClientConfig(streaming=streaming, httpx_client=self.http)).create(card)

    async def adk_turn(self, runner, session, text):
        events = [event async for event in runner.run_async(
            user_id="test", session_id=session.id,
            new_message=types.Content(role="user", parts=[types.Part(text=text)]),
        )]
        self.assertFalse([event.error_message for event in events if event.error_code])
        return events

    async def adk_runner(self, name="Echo", streaming=True):
        runner = InMemoryRunner(agent=create_agent(
            self.base_url, name, streaming=streaming, http_client=self.http,
        ), app_name=APP_NAME)
        self.addAsyncCleanup(runner.close)
        session = await runner.session_service.create_session(app_name=runner.app_name, user_id="test")
        return runner, session

    async def test_discovery(self):
        directory = await self.http.get(f"{self.base_url}/api/agui/a2a/")
        directory.raise_for_status()
        self.assertEqual({entry["name"] for entry in directory.json()["agents"]}, {"Echo", "Report"})
        for name in ("Echo", "Report"):
            endpoint = f"{self.base_url}/api/agui/a2a/{name}"
            card = await A2ACardResolver(self.http, endpoint).get_agent_card()
            self.assertEqual(card.name, name)
            self.assertTrue(card.capabilities.streaming)
            self.assertEqual(card.supported_interfaces[0].url, endpoint)
            self.assertEqual(card.supported_interfaces[0].protocol_version, "1.0")

    async def test_adk_text_reply(self):
        for streaming in (False, True):
            with self.subTest(streaming=streaming):
                runner, session = await self.adk_runner(streaming=streaming)
                events = await self.adk_turn(runner, session, "Hello ADK — नमस्ते 👋")
                final = [value for event in events if not event.partial for value in event_values(event)]
                self.assertEqual(final, ["Hastekit says: Hello ADK — नमस्ते 👋"])
                self.assertIn("SendStreamingMessage" if streaming else "SendMessage", self.methods)
                if streaming:
                    self.assertTrue(any(event.partial for event in events))

    async def test_adk_json_artifact(self):
        for streaming in (False, True):
            with self.subTest(streaming=streaming):
                runner, session = await self.adk_runner("Report", streaming)
                events = await self.adk_turn(runner, session, "Build a report")
                final = [value for event in events if not event.partial for value in event_values(event)]
                self.assertEqual(final, [report("Build a report")])

    async def test_adk_conversation(self):
        runner, session = await self.adk_runner()
        await self.adk_turn(runner, session, "remember-apricot")
        events = await self.adk_turn(runner, session, "history")
        final = [value for event in events if not event.partial for value in event_values(event)]
        self.assertEqual(final, ["remember-apricot | history"])
        # Another ADK session must not inherit the first session's history.
        other = await runner.session_service.create_session(app_name=runner.app_name, user_id="test")
        events = await self.adk_turn(runner, other, "history")
        self.assertEqual([v for e in events if not e.partial for v in event_values(e)], ["history"])

    async def test_task_artifact_and_retrieval(self):
        for name in ("Echo", "Report"):
            with self.subTest(agent=name):
                client = await self.client(name)
                results = [event async for event in client.send_message(request("artifact"))]
                self.assertEqual(len(results), 1)
                task = results[0].task
                self.assertEqual(task.status.state, TaskState.TASK_STATE_COMPLETED)
                self.assertTrue(task.id)
                self.assertTrue(task.context_id)
                self.assertEqual(len(task.artifacts), 1)
                artifact = task.artifacts[0]
                self.assertTrue(artifact.artifact_id)
                self.assertEqual(len(artifact.parts), 1)
                part = artifact.parts[0]
                if name == "Echo":
                    self.assertEqual(part.text, "Hastekit says: artifact")
                else:
                    self.assertEqual(MessageToDict(part.data), report("artifact"))
                fetched = await client.get_task(GetTaskRequest(id=task.id))
                self.assertEqual(fetched, task)

    async def test_stream_artifact_replacement(self):
        for name in ("Echo", "Report"):
            with self.subTest(agent=name):
                client = await self.client(name, streaming=True)
                events = [event async for event in client.send_message(request("stream"))]
                updates = [e.artifact_update for e in events if e.HasField("artifact_update")]
                self.assertGreater(len(updates), 2)
                self.assertEqual(len({u.artifact.artifact_id for u in updates}), 1)
                self.assertFalse(updates[0].append)
                self.assertTrue(all(u.append for u in updates[1:-1]))
                self.assertFalse(updates[-1].append)
                self.assertTrue(updates[-1].last_chunk)
                self.assertTrue(all(not u.last_chunk for u in updates[:-1]))
                self.assertEqual(events[-1].status_update.status.state, TaskState.TASK_STATE_COMPLETED)
                task = await client.get_task(GetTaskRequest(id=updates[-1].task_id))
                self.assertEqual(list(task.artifacts), [updates[-1].artifact])
                # Full final replacement must not append duplicate provisional text.
                self.assertEqual(len(task.artifacts[0].parts), 1)

    async def test_failed_execution(self):
        for streaming in (False, True):
            client = await self.client(streaming=streaming)
            results = [event async for event in client.send_message(request("fail"))]
            status = results[-1].status_update.status if streaming else results[-1].task.status
            self.assertEqual(status.state, TaskState.TASK_STATE_FAILED)
            self.assertEqual(status.message.parts[0].text, "Agent execution failed.")

    async def test_adk_surfaces_failed_task(self):
        for streaming in (False, True):
            with self.subTest(streaming=streaming):
                runner, session = await self.adk_runner(streaming=streaming)
                with self.assertRaisesRegex(RuntimeError, "TASK_STATE_FAILED"):
                    await self.adk_turn(runner, session, "fail")

    async def test_json_input(self):
        client = await self.client("Report")
        req = request("")
        req.message.parts[0].ClearField("text")
        ParseDict({"question": "hello", "count": 2}, req.message.parts[0].data)
        events = [event async for event in client.send_message(req)]
        task = events[-1].task
        self.assertEqual(task.status.state, TaskState.TASK_STATE_COMPLETED)
        payload = MessageToDict(task.artifacts[0].parts[0].data)
        self.assertEqual(json.loads(payload["request"]), {"question": "hello", "count": 2})

    async def test_unsupported_file_input(self):
        # Explicit protocol error, not a successful task that silently drops input.
        result = await self.http.post(f"{self.base_url}/api/agui/a2a/Echo", json={
            "jsonrpc": "2.0", "id": "unsupported-file", "method": "SendMessage",
            "params": {"message": {"messageId": str(uuid.uuid4()), "role": "ROLE_USER",
                                   "parts": [{"raw": "aGVsbG8=", "mediaType": "text/plain"}]}},
        })
        result.raise_for_status()
        self.assertEqual(result.json()["error"]["code"], -32602)

    async def test_adk_exposed_a2a_chain(self):
        # Client -> ADK A2A server -> ADK remote agent -> Hastekit A2A server.
        # Both hops use genuine network sockets and standard framework adapters.
        for name in ("Echo", "Report"):
            with self.subTest(agent=name):
                sock = socket.socket()
                sock.bind(("127.0.0.1", 0))
                port = sock.getsockname()[1]
                app = create_a2a_app(self.base_url, name, http_client=self.http, port=port)
                server = uvicorn.Server(uvicorn.Config(app, log_level="error", lifespan="on"))
                serving = asyncio.create_task(server.serve(sockets=[sock]))
                try:
                    async with asyncio.timeout(15):
                        while not server.started:
                            if serving.done():
                                await serving
                                self.fail("ADK server stopped before startup")
                            await asyncio.sleep(0.01)
                    card = await A2ACardResolver(self.http, f"http://127.0.0.1:{port}").get_agent_card()
                    client = ClientFactory(ClientConfig(streaming=False, httpx_client=self.http)).create(card)
                    events = [e async for e in client.send_message(request("two hops"))]
                    task = events[-1].task
                    self.assertEqual(task.status.state, TaskState.TASK_STATE_COMPLETED)
                    self.assertEqual(len(task.artifacts), 1)
                    parts = task.artifacts[0].parts
                    self.assertEqual(len(parts), 1)
                    if name == "Echo":
                        self.assertEqual(parts[0].text, "Hastekit says: two hops")
                    else:
                        self.assertEqual(MessageToDict(parts[0].data), report("two hops"))
                finally:
                    server.should_exit = True
                    try:
                        await asyncio.wait_for(serving, timeout=15)
                    finally:
                        sock.close()


if __name__ == "__main__":
    unittest.main(verbosity=2)

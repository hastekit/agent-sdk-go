"""Google ADK coordinator that delegates to Hastekit over A2A 1.0."""

import argparse
import asyncio
import json
from contextlib import asynccontextmanager
from pathlib import Path

import httpx
from a2a.client import ClientConfig, ClientFactory
from google.adk.agents import BaseAgent
from google.adk.agents.remote_a2a_agent import RemoteA2aAgent
from google.adk.a2a.converters.part_converter import (
    A2A_DATA_PART_END_TAG,
    A2A_DATA_PART_START_TAG,
)
from google.adk.runners import InMemoryRunner
from google.genai import types

APP_NAME = Path(__file__).resolve().parent.name


class CoordinatorAgent(BaseAgent):
    """Delegate the turn to the remote agent and preserve its returned events."""

    async def _run_async_impl(self, ctx):
        failure = None
        async for event in self.sub_agents[0].run_async(ctx):
            # ADK 2.11 preserves remote failure in A2A metadata, but does not
            # necessarily set Event.error_code. Do not forward it as success.
            response = (event.custom_metadata or {}).get("a2a:response", {})
            state = response.get("status", {}).get("state")
            if state in {"TASK_STATE_FAILED", "TASK_STATE_CANCELED", "TASK_STATE_REJECTED"}:
                failure = RuntimeError(f"Hastekit task ended with {state}: {event_values(event)}")
            elif failure is None:
                yield event
        # Let the remote iterator unwind its tracing/transport contexts before
        # raising; abandoning it at its last yield leaks ADK tracing contexts.
        if failure is not None:
            raise failure


def create_agent(base_url, agent_name="Echo", *, http_client, streaming=True):
    """Use ADK's standard discovery, transport, and event converters unchanged."""
    remote = RemoteA2aAgent(
        name="hastekit_remote",
        description="Ask the Hastekit agent for a reply or a structured report.",
        agent_card=f"{base_url.rstrip('/')}/api/agui/a2a/{agent_name}/.well-known/agent-card.json",
        a2a_client_factory=ClientFactory(ClientConfig(
            streaming=streaming, httpx_client=http_client,
        )),
    )
    # Custom agents do not need an LLM to delegate. This is a real ADK agent
    # and Runner, with the remote Hastekit agent as its A2A sub-agent.
    return CoordinatorAgent(name="adk_coordinator", sub_agents=[remote])


def create_a2a_app(base_url, agent_name="Echo", *, port=8088, http_client=None):
    from google.adk.a2a.utils.agent_to_a2a import to_a2a

    client = http_client or httpx.AsyncClient(timeout=30, trust_env=False)
    agent = create_agent(base_url, agent_name, http_client=client)
    runner = InMemoryRunner(agent=agent, app_name=APP_NAME)

    @asynccontextmanager
    async def lifespan(app):
        try:
            yield
        finally:
            try:
                await runner.close()
            finally:
                if http_client is None:
                    await client.aclose()

    return to_a2a(agent, host="127.0.0.1", port=port, runner=runner, lifespan=lifespan)


def event_values(event):
    """Read text and generic A2A DataParts from ADK's native event format."""
    values = []
    for part in event.content.parts if event.content else []:
        if part.text:
            values.append(part.text)
        elif part.inline_data:
            data = part.inline_data.data
            if data.startswith(A2A_DATA_PART_START_TAG) and data.endswith(A2A_DATA_PART_END_TAG):
                values.append(json.loads(data[len(A2A_DATA_PART_START_TAG):-len(A2A_DATA_PART_END_TAG)]))
    return values


async def chat(args):
    async with httpx.AsyncClient(timeout=30, trust_env=False) as http_client:
        runner = InMemoryRunner(agent=create_agent(
            args.base_url, args.agent, streaming=not args.no_stream, http_client=http_client,
        ), app_name=APP_NAME)
        try:
            session = await runner.session_service.create_session(
                app_name=runner.app_name, user_id="demo",
            )
            async for event in runner.run_async(
                user_id="demo", session_id=session.id,
                new_message=types.Content(role="user", parts=[types.Part(text=args.message)]),
            ):
                if event.error_code:
                    raise RuntimeError(f"{event.error_code}: {event.error_message}")
                # Partial events are provisional; the final artifact replaces
                # them. Printing both would duplicate the completed reply.
                if not event.partial:
                    for value in event_values(event):
                        print(value if isinstance(value, str) else json.dumps(value, indent=2, ensure_ascii=False))
        finally:
            await runner.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("message", nargs="?", default="Hello from Google ADK!")
    parser.add_argument("--base-url", default="http://127.0.0.1:8087")
    parser.add_argument("--agent", choices=["Echo", "Report"], default="Echo")
    parser.add_argument("--no-stream", action="store_true")
    parser.add_argument("--serve", action="store_true", help="Expose the ADK coordinator as another A2A server")
    parser.add_argument("--port", type=int, default=8088)
    args = parser.parse_args()
    if args.serve:
        import uvicorn
        app = create_a2a_app(args.base_url, args.agent, port=args.port)
        uvicorn.run(app, host="127.0.0.1", port=args.port)
    else:
        asyncio.run(chat(args))


if __name__ == "__main__":
    main()

# Google ADK ↔ Hastekit over A2A

A runnable, credential-free interoperability example using Google ADK **2.11.0**,
Python `a2a-sdk` **1.2.2**, and Hastekit's Go A2A adapter (`a2a-go/v2` **2.5.0**).
Both sides speak **A2A 1.0 JSON-RPC**, including SSE streaming.

```text
ADK Runner → CoordinatorAgent → RemoteA2aAgent
                                  │ agent card + A2A HTTP requests
                                  ▼
                         Hastekit Agent (Go)
                                  │
                         text / JSON artifact
```

`main.go` runs two real Hastekit agents with the normal local execution loop,
conversation history, stream broker, and A2A server. A deterministic model
implementation supplies streamed responses so protocol tests need no model API
keys. `Echo` responds to the supplied message; `Report` returns a structured JSON
artifact. The special messages `history` and `fail` exercise conversation history
and execution errors.

`adk_agent.py` builds a custom ADK `BaseAgent` that delegates to Hastekit through
ADK's unmodified `RemoteA2aAgent`. It can also expose the coordinator using ADK's
`to_a2a`, enabling a second A2A hop. These tests verify framework and protocol
interoperability, not a live LLM's reasoning or tool selection.

## Run the integration tests

From this directory, with Go 1.25.3+ and Python 3.10+ (tested locally with 3.14.5):

```sh
python3 -m venv .venv
.venv/bin/python -m pip install -r requirements.txt
.venv/bin/python test_interop.py
```

The harness builds the Go server, starts it on an available loopback port, starts
temporary ADK servers for the two-hop tests, and shuts everything down afterward.
No separately running server or external model service is required. To test an
already running copy of this demo, set `HASTEKIT_A2A_URL=http://127.0.0.1:8087`.
The tests expect this example's Echo/Report behavior.

The 11 integration tests cover:

- Directory and standard agent-card discovery, including A2A protocol version.
- ADK text replies in blocking and streaming modes, including Unicode.
- ADK structured JSON artifacts in blocking and streaming modes.
- Multi-turn conversation continuity and isolation between ADK sessions.
- Task IDs, context IDs, artifact IDs, exact artifact contents, and `GetTask`.
- Streaming append flags, final replacement, and completion without duplication.
- Failed execution through the Python A2A client and the ADK coordinator.
- JSON data input and explicit rejection of unsupported binary/file input.
- A2A client → ADK server → Hastekit server, returning text and JSON artifacts.

The repository CI also runs this integration suite. The existing Go A2A tests
cover additional behaviors such as cancellation, input-required continuation,
authorization, and namespace isolation:

```sh
go test ./pkg/agents ./pkg/agui/... -run A2A -count=1
```

## Run the agents manually

From the repository root, start Hastekit:

```sh
go run ./examples/agents/16_a2a_adk
```

In another terminal, from this example directory:

```sh
.venv/bin/python adk_agent.py 'Hello from Google ADK!'
# Hastekit says: Hello from Google ADK!

.venv/bin/python adk_agent.py --agent Report 'Build a report'
# {"agent": "hastekit", "request": "Build a report", "status": "ok",
#  "items": ["text replies", "JSON artifacts"]}

.venv/bin/python adk_agent.py --no-stream 'Use blocking SendMessage'
```

To expose the ADK coordinator as an A2A server as well:

```sh
.venv/bin/python adk_agent.py --serve --agent Report --port 8088
```

Its agent card is at `http://127.0.0.1:8088/.well-known/agent-card.json`. Send it an
A2A request to forward through ADK to Hastekit:

```sh
curl http://127.0.0.1:8088/ \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":"demo","method":"SendMessage","params":{"message":{"messageId":"demo-1","role":"ROLE_USER","parts":[{"text":"Build a report"}]}}}'
```

## Artifact and error semantics

Hastekit returns completed A2A **Tasks with artifacts**. Text is a text part;
structured output is a JSON data part. Streaming sends provisional text deltas
and then replaces them with the authoritative final artifact (`append=false`,
`lastChunk=true`). Consume final ADK events for the completed answer; concatenating
partial and final events duplicates content.

ADK represents generic A2A JSON data parts as tagged inline blobs. `event_values`
reads that documented-in-source ADK representation for display and assertions;
the protocol adapters themselves are unchanged. Passing the events through
ADK's A2A server converts them back to real JSON data parts.

ADK 2.11 may carry remote task failure in `custom_metadata["a2a:response"]` without
setting `Event.error_code`. The coordinator checks terminal failure states and
raises after the remote iterator finishes, preserving cleanup and preventing a
failed task from being presented as a successful reply.

This example verifies text and JSON artifacts. Hastekit's adapter does **not**
currently transport file/binary artifacts or accept file inputs. It uses
process-local A2A task storage. Live model behavior, durable restarts, and
deployment authentication are outside this suite. ADK currently labels its A2A
integration experimental; the tested direct dependency versions are pinned.

References: [ADK remote-agent guide](https://google.github.io/adk-docs/a2a/quickstart-consuming/),
[ADK exposing guide](https://google.github.io/adk-docs/a2a/quickstart-exposing/),
and [Hastekit A2A documentation](../../../pkg/agui/A2A.md).

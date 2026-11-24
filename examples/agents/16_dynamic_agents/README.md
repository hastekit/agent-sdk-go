# Dynamic agents

Agents as data: each namespace (a user or a tenant) creates its own agents at
run time, and they run locally, on Temporal or on Restate without registering
anything new on the worker.

```sh
OPENAI_API_KEY=... go run ./examples/agents/16_dynamic_agents                    # in process
OPENAI_API_KEY=... go run ./examples/agents/16_dynamic_agents -runtime temporal  # Temporal on :7233, Redis on :6379
OPENAI_API_KEY=... go run ./examples/agents/16_dynamic_agents -runtime restate   # Restate ingress on :8080, Redis on :6379
```

For Restate, register the service once:
`curl localhost:9070/deployments -H 'content-type: application/json' -d '{"uri":"http://host.docker.internal:9080"}'`.

## What is where

- **`agentconfig.Config`** is the agent: name, description, model id,
  parameters, instruction, max loops, output schema, and *picks* — skill names,
  MCP connectors with their tools, catalog tool names, handoffs, and
  sub-agents — other configs it calls as tools, keeping the conversation. It is
  plain JSON.
- **Stores** hold configs per namespace. The empty namespace holds built-ins
  (`agentconfig.NewInlineStore`), which every namespace can run and none can
  replace. `NewFileStore` and `NewMemoryStore` hold each namespace's own.
- **`agentconfig.Builder`** holds everything a config may choose among, in
  code: the model allowlist, the unfiltered skill and MCP stores, the tool
  catalog, history and middlewares. It builds a plain `agents.NewAgent`, giving
  it only the config's picks through `skills.Filter` and `mcpclient.Filter`.
- **Durable runs** take a snapshot of the config as workflow input. One
  generic `AgentBuilder` workflow serves every config, so an agent written
  after the worker started runs durably; an edit applies from the next run.
- **`agentconfig.Registry`** is the `agui.Registry`: it finds code agents and
  configs for the request's namespace only. It is also a
  `routines.AgentRegistry`, so a namespace's routines can target its own
  agents: `routines.NewService(store, &routines.AgentExecutor{Registry: registry})`.
- **Background tools** in the catalog work everywhere: in process, on
  Temporal (a background workflow per task) and on Restate (a background
  service invocation per task). A task's result starts a new run from the
  snapshot of the run that started it.

## Try it

Open http://localhost:8070 and choose **Agents** in the sidebar (or **New or
edit agents…** in the agent menu) to create one: pick a model, tools, skills,
MCP connectors and handoffs from what the server offers, then **Create and
chat**. The rest of this section does the same over HTTP.

Create an agent in the `alice` namespace (the demo trusts an `X-User` header;
derive it from authentication in a real server):

```sh
curl -X PUT localhost:8070/api/agui/agent-configs/timekeeper \
  -H 'X-User: alice' -H 'content-type: application/json' \
  -d '{"model":"OpenAI/gpt-4.1-mini","instruction":"You tell the time.","tools":["current_time"]}'
```

It is listed for `alice`, and not for `bob`:

```sh
curl localhost:8070/api/agui/agents -H 'X-User: alice'   # ["concierge","timekeeper"]
curl localhost:8070/api/agui/agents -H 'X-User: bob'     # ["concierge"]
```

Run it:

```sh
curl -N localhost:8070/api/agui/agents/timekeeper/run -H 'X-User: alice' \
  -H 'content-type: application/json' \
  -d '{"threadId":"t1","messages":[{"id":"m1","role":"user","content":"What time is it in Tokyo?"}]}'
```

Writes are checked before they are stored: a model off the allowlist or a tool
outside the catalog is refused with 400, and a name a built-in holds
(`concierge`) with 409. No request can write the built-in namespace.

# Dynamic skill sets

From the repository root:

```sh
OPENAI_API_KEY=... go run ./examples/agents/15_dynamic_skills
OPENAI_API_KEY=... go run ./examples/agents/15_dynamic_skills -serve
```

The example combines a filesystem source with all skills enabled by default with a callback-based
team catalog. The callbacks can list and read from your database, object store,
or HTTP service. The embedded UI runs at http://localhost:8080.

The example also configures a persistent `library` source using `pkg/skills`.
Use **Skills → Manage skill library** to upload a skill folder, browse saved skills,
and preview instructions. Uploaded skills are opt-in. Pass `-skill-store /path`
to choose the storage directory (default `./data/skills`). For S3 and API details,
see [persistent skills](../../../pkg/skills/README.md).

`AgentConfig.Skills` is a list of sources. Each source implements `GetName`, `ListSkills`,
and `ResolveSkill`; `SkillSetFuncs` provides callbacks for these methods.
`NewFilesystemSkillSet` loads a folder; `NewFSSkillSet` accepts embedded or other
`fs.FS` sources. Both enable every skill by default and discover catalog changes
on each run.

| Configuration | Default | User override |
| --- | --- | --- |
| `Required: true` | Enabled | Cannot disable |
| `DefaultEnabled: true` | Enabled | Can disable |
| Neither flag (zero value) | Disabled | Can enable |

Select skills using `Input.Skills.Enable` and `Input.Skills.Disable`, with names
such as `release-review`. Disable wins when an optional skill appears in
both lists. Unknown names are ignored, allowing handoffs between agents with
different catalogs. Required means available to the model; it does not force a
read or inject the full instructions.

The agent lists each source once when entering its loop. Only enabled metadata
enters the prompt. A single `read_skill` tool routes names to their source, checks
the run's enabled catalog, and restricts file reads to `SKILL.md` and the declared
`Resources`. An empty `file` reads the instructions. Use the prompt's
`ResolveSkills` resolver (included in `DefaultResolvers`) to advertise skills.

The catalog is a per-run snapshot, but content is resolved on demand. The built-in store
keeps only the latest bundle, without versions or aliases. Callbacks must be
concurrency-safe, honor cancellation, and enforce application tenant boundaries.
A failed listing fails the run. Selection is not persisted in conversation
history: resend it for every new turn or approval resume. The embedded UI remembers
choices per agent in browser storage. SSE reconnection does not change the active
run. Background follow-up runs inherit the originating selection.

`AgentConfig.Skills` accepts `[]SkillSet`. The former `SkillProvider` API has been
removed. Use filesystem/embedded sources from `pkg/skills`; the agent creates
one reader for all enabled sources. Registry adapters and standalone reader
constructors have been removed.

Temporal records listing and reads as activities; Restate journals them as run
steps. Set identities are part of worker configuration, while their catalogs can
change without rebuilding the agent. Register workers before accepting runs.

For custom AG-UI clients:

- `GET /api/agui/agents/{agent}/skills` returns the visible catalog and default state.
- Send `forwardedProps.skills: { enable: ["release-review"], disable: [] }` on
  runs and approval resumes. Required skills are enforced server-side.
- The list endpoint provides `Namespace` and `Header` in the resolver's run context;
  the run endpoint provides these plus `State`, `Context`, and `ForwardedProps`.
  Use server-controlled namespace and your authentication layer for tenant access.

Skill policies control instruction availability. They do not grant or revoke tool
permissions, and they cannot erase skill text already stored in conversation history.

Skill names have no source prefix. Global skills (`Global: true`) always win
against user skills with the same name. Filesystem sources are global. Within the
same scope, later sources win. Selection is applied after merging. Source names
must still be unique for runtime registration.

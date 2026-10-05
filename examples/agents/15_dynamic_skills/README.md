# Dynamic skills

From the repository root:

```sh
OPENAI_API_KEY=... go run ./examples/agents/15_dynamic_skills
OPENAI_API_KEY=... go run ./examples/agents/15_dynamic_skills -serve
```

The agent's `SkillClient` combines two kinds of skills, the same way MCP servers work:

- **Global skills** belong to the developer and are configured per agent with
  `skills.NewClient(store).WithGlobalSkills(sources...)`. This example uses a
  folder (`skills.NewDirSource`) and `teamSkills`, a custom `skills.Source` whose
  `List` and `Read` could call your database, object store, or HTTP service.
  Globals are always on, and users cannot turn them off or replace them.
- **User skills** are the ones each namespace uploads to the shared `Store`
  (`./data/skills` by default; pass `-skill-store /path` to change it). They are
  on unless the run disables them, and cannot reuse a global skill's name.

In the embedded UI at http://localhost:8080, use **+ → Skills → Manage Skills**
to upload a skill folder, browse saved skills, and preview instructions. The
Skills menu lists the user's own skills for every agent and lets them switch any
off; global skills are always on and not listed.
For S3 and API details, see [persistent skills](../../../pkg/agents/skills/README.md).

Disable skills with `Input.Skills.Disable`, using names such as `my-drafts`.
Naming a global skill there has no effect. Unknown names are ignored, allowing
handoffs between agents with different catalogs.

The agent lists skills once when entering its loop. Only enabled metadata enters
the prompt. A single `read_skill` tool checks the run's enabled catalog and
restricts file reads to `SKILL.md` and the declared `Resources`. An empty `file`
reads the instructions. Use the prompt's `ResolveSkills` resolver (included in
`DefaultResolvers`) to advertise skills.

The catalog is a per-run snapshot, but content is read on demand. The built-in
store keeps only the latest bundle, without versions or aliases. Sources must be
concurrency-safe and honor cancellation. A failed listing fails the run.
Selection is not persisted in conversation history: resend it for every new turn
or approval resume. The embedded UI remembers choices once per browser, for
every agent. Background follow-up runs inherit the originating selection.

Temporal records listing and reads as the agent's `_Skills_ListSkills` and
`_Skills_ReadSkill` activities; Restate journals them as run steps. The catalog
can change without rebuilding the agent or re-registering workers.

For custom AG-UI clients:

- `GET /api/agui/skills` lists the user's own skills. It is not scoped to an
  agent: every agent using the store reads the same skills. It returns 404 when
  no skill store is configured.
- Send `forwardedProps.skills: { disable: ["my-drafts"] }` on runs and approval
  resumes. Global skills stay on server-side, so naming one has no effect.
- The run endpoint provides `Namespace`, `Header`, `State`, `Context`, and
  `ForwardedProps` in the run context. Use a server-controlled namespace and
  your authentication layer for tenant access.

Skill selection controls instruction availability. It does not grant or revoke
tool permissions, and it cannot erase skill text already stored in conversation history.

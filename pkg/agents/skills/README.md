# Persistent skills

`skills.Store` lists and reads skill bundles by namespace: `Get` and paginated
`List`. `skills.MutableStore` is a store that also saves them, with `Put` and
idempotent `Delete`. The names match MCP's `MCPServerConfigStore` and
`MutableMCPServerConfigStore`. Implement either for your own database or
service, or use the filesystem and S3 stores.

A bundle contains `SKILL.md` and supporting files keyed by paths relative to the
skill folder. `SKILL.md` must have YAML frontmatter with an explicit `name` and
`description`. Availability flags in uploaded frontmatter are ignored. Bundles are limited
to 100 files and 10 MiB of decoded content; absolute and traversing paths are rejected.
The HTTP upload limit is 20 MiB including multipart or base64 JSON overhead.

## Global and user skills

`skills.Client` is an agent's `SkillClient`. It works like `mcpclient.Client`:
`NewClient(stores...)` reads every store, and the namespace says who owns a
skill.

- **Global skills** live in a store's empty namespace and belong to the
  developer. They are always on: users cannot turn them off, and a global shadows
  a user skill with the same name. Two stores holding one global name is an error.
- **User skills** live in the namespace that saved them. They are on unless a run
  disables them with `Input.Skills.Disable`. An empty namespace lists globals only.

Only trusted server code may write the empty namespace: it changes every
agent's globals. The `/skills` routes refuse an empty or blank namespace, and
stores reject a blank one so it cannot pass for the global namespace.

Globals can sit in the same store as users' uploads, written by your application
with `store.Put(ctx, "", bundle)`, or in read-only stores that serve only the
empty namespace and never accept writes:

| Constructor | Serves as global skills |
| --- | --- |
| `NewDirStore(directory)` | Ordinary skill folders, rediscovered on every listing |
| `NewFSStore(fsys)` | An `embed.FS` or other `fs.FS`, the same way |
| `NewInlineStore(bundles...)` | Bundles defined in code, validated and copied at construction |

Implement `Store` (`List`, `Get`) for a database or service. The agent selects
skills, builds the prompt metadata, and creates one `read_skill` tool. A request
for instructions returns the body without YAML frontmatter; an explicit
`file: "SKILL.md"` returns the original document. Supporting files are read as-is.

```go
store, err := skills.NewFileStore("./data/skills") // users' uploads, shared by every agent
if err != nil { return err }
reviewSkills, err := skills.NewDirStore("./skills/review")
if err != nil { return err }
styleGuide, err := skills.NewFSStore(embeddedStyle)
if err != nil { return err }

reviewer := &hastekit.AgentConfig{Name: "Reviewer", SkillClient: skills.NewClient(store, reviewSkills)}
writer := &hastekit.AgentConfig{Name: "Writer", SkillClient: skills.NewClient(store, styleGuide)}
// Set each agent's LLM and instruction before constructing it.
```

## Picking skills per agent

`Filter(store, names, options...)` lists and reads only the named skills of a
store, global or the user's own, so an agent can take part of a shared store.
Wrap every store the agent reads:

```go
everything := skills.NewClient(store, builtins) // the default agent
picks := []string{"release-review", "my-meeting-notes"} // a built-in and an upload
custom := skills.NewClient(skills.Filter(store, picks), skills.Filter(builtins, picks))
```

Users' skills the list does not name are dropped, including skills they upload
later; pass `KeepUserSkills()` to keep them, for an agent that narrows only the
globals. Precedence is unchanged: when a kept name exists in both scopes the
global wins, and a global left out does not shadow a kept user skill. Reads of
anything else return `ErrNotFound`, names nothing lists are ignored, filtering a
filtered store keeps names both share, and an empty list keeps nothing.
Per-run `Input.Skills.Disable` still applies to the picks.

Storage is shared across agents within the caller's namespace; neither the agent
name nor the client creates a storage scope. Agents without a `SkillClient` get no
skills. Uploads, replacements, and deletions are visible on the next catalog
listing or run; agents do not need to be reconstructed.

## Embedded UI

```go
store, err := skills.NewFileStore("./data/skills")
if err != nil { return err }

agent, err := hastekit.NewAgent(&hastekit.AgentConfig{
    Name: "Assistant",
    LLM: model,
    SkillClient: skills.NewClient(store),
    Instruction: hastekit.NewPrompt("Use the relevant skills.",
        prompts.WithResolver(prompts.DefaultResolvers()...)),
})
if err != nil { return err }
registry := hastekit.NewRegistry()
if err := registry.Register(agent); err != nil { return err }
return web.Serve(":8080", registry, agui.WithSkillStore(store))
```

Imports: `pkg/agents/skills`, `pkg/agents/prompts`, `pkg/agui`, `pkg/agui/web`,
and the root SDK as `hastekit`. `model` is your configured provider. Protect the
HTTP handler with your application's authentication and management authorization
before exposing it publicly; `web.Handler` can be wrapped with that middleware.

The sidebar's **Skill library** action (also available through the composer's
**+ → Skills → Manage Skills**) uploads a skill folder or selected files, lists
stored skills, previews SKILL.md as text, downloads resources, and deletes skills
with an inline confirmation. The upload button replaces a same-named skill's
entire bundle. The composer's Skills menu lists the same library, which is not
scoped to an agent: a new upload is on for every agent using this store until
the user turns it off there, and that choice applies to every agent. The
agent's global skills are listed too, in the menu and the library, marked
"Built in · always on" and never switchable or deletable; the UI reads them from
`GET /agents/{agent}/skills`. Neither list reflects a `Filter`: the library shows
all of the user's uploads whichever skills an agent was given.

Run `go run ./examples/agents/15_dynamic_skills -serve -skill-store /tmp/skills`
from the repository root to try it. Model calls require `OPENAI_API_KEY`; uploading
and browsing skills do not call the model.

## S3

```go
store, err := skills.NewS3Store(s3Client, skills.S3Config{
    Bucket: "private-agent-assets",
    Prefix: "skills",
})
if err != nil { return err }
client := skills.NewClient(store)
```

`s3Client` is an AWS SDK v2 S3 client. Credentials, region, endpoint, bucket creation,
encryption and lifecycle remain application-owned. Use a dedicated prefix. The
client needs PutObject, GetObject, ListObjectsV2 and DeleteObject permissions.
Each complete bundle occupies one JSON object. Listing follows S3 continuation
tokens and reads bundle metadata from the objects on a cache miss.
A shared S3 store can be used by HTTP servers and durable workers in different processes.

The filesystem store also uses one JSON file per bundle. Replacements use a temporary
file and atomic rename; S3 replaces one object atomically. Readers see a complete
old or new bundle. Concurrent replacements use last-writer-wins semantics. The
filesystem root must be application-owned, not writable by untrusted processes.
This persistent format is separate from `NewDirStore`, which reads an
existing tree of ordinary skill folders without an upload API.

## Listing cache

Both built-in stores cache unfiltered listing pages, before the agent applies
policies or input selections. The default is a private in-memory cache with a
one-minute TTL. File contents are still read from persistence on demand.

Configure either store with the same options:

```go
store, err := skills.NewFileStore("./data/skills",
    skills.WithCache(cache.NewMemoryCache()),
    skills.WithCacheTTL(5*time.Minute),
)
```

For shared invalidation across processes, supply Redis:

```go
redisClient := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
// The application owns and closes redisClient.
sharedCache, err := cache.NewRedisCache(redisClient, "my-app:skills:")
if err != nil { return err }
store, err := skills.NewS3Store(s3Client, skills.S3Config{
    Bucket: "private-agent-assets", Prefix: "skills",
}, skills.WithCache(sharedCache), skills.WithCacheTTL(5*time.Minute))
```

`cache` is `github.com/hastekit/agent-sdk-go/pkg/cache`; `redis` is `github.com/redis/go-redis/v9`. Use the same cache prefix and backing
storage location across replicas; use distinct cache prefixes for unrelated
deployments or S3-compatible endpoints with overlapping bucket names.

Custom implementations only need the key-value contract:

```go
 type KeyValueStore interface {
     Get(context.Context, string) ([]byte, bool, error)
     Set(context.Context, string, []byte, time.Duration) error
     Delete(context.Context, string) error
 }
```

The store owns cache keys, serialization, TTL, and invalidation. Keys isolate the
storage location, namespace, cursor, and page size. Uploads, replacements, and
deletes switch the namespace's version key, making all older pages unreachable;
old pages expire naturally. A listing that overlaps a write cannot repopulate the
new version with stale data. Expired or evicted version keys get fresh random
versions, so they cannot resurrect older listings.

In-memory invalidation is shared only by stores using the same cache instance.
Redis invalidation is shared by all replicas configured with the same cache.
Direct filesystem/S3 writes, or writes through a different cache, become visible
when the configured TTL expires. Pass `WithCache(nil)` or `WithCacheTTL(0)` to
bypass caching. Cache errors are returned; if invalidation fails after a backing
write, the operation reports that the write may already have been applied.

## Namespaces

Management routes use the authenticated namespace from
`agui.WithNamespaceResolver`. Request bodies and query parameters cannot select
another namespace. An agent execution passes `Input.Namespace` explicitly to
skill listing and reading. Direct calls use `ListSkills(ctx, namespace, runContext)`
and `ReadSkill(ctx, namespace, runContext, name, file)`. To see one agent's full
catalog, globals included, call `agent.ListSkills(ctx, namespace, runContext, selection)`
in Go; the HTTP API lists only the user's own skills. `RunContext` remains
application data; a key named `Namespace` has no special meaning.

To manage a shared library at run time, keep it in a store's empty namespace and
populate it through trusted server-side calls such as `store.Put(ctx, "", bundle)`,
never through the user routes, which refuse that namespace.

New runs refresh the catalog. An active run's enabled names and resource allowlist
remain fixed; content reads see the latest saved bundle. The store keeps only the
latest bundle; there are no skill versions or aliases. Deleting a skill makes later
reads fail; it does not erase previously read text from conversation history.
Temporal and Restate run listing and reads as activities or run steps.

## HTTP API

Pass `agui.WithSkillStore(store)` to `web.Serve` or `web.Handler` to enable management. Omit it to disable management. The **+ → Skills → Manage Skills** button opens uploads and browsing. The embedded UI mounts these below `/api/agui`:

| Method | Path | Behavior |
| --- | --- | --- |
| GET | `/skills?limit=50&cursor=...` | The caller's own skills, with opaque `nextCursor`; the Skills menu reads this too |
| POST | `/skills` | Upload/replace a complete bundle |
| PUT | `/skills/{name}` | Replace via JSON; name must match frontmatter |
| GET | `/skills/{name}` | JSON bundle, files encoded as base64 |
| GET | `/skills/{name}?file=refs/check.md` | Download a bundled file |
| DELETE | `/skills/{name}` | Idempotently remove the bundle |

POST supports `multipart/form-data` with files whose `filename` is the relative
resource path. A folder upload strips the enclosing folder name. Exactly one
SKILL.md must be at the bundle root; uploading a parent folder of multiple skills
is not supported. ZIP extraction is not performed.

POST and PUT also accept `application/json`: `{"files":{"SKILL.md":"<base64>"}}`.
Success returns metadata. Listing limits are 1–200. Errors use 400 for invalid
content, 403 for namespace denial, 404 for missing content, 409 for a name
reserved by a global skill, and 413 for size limits.
Backend errors return 500 without exposing storage details. File responses are
attachments with `nosniff`; the UI never executes uploaded scripts or renders
uploaded HTML.

Uploads whose name matches a global skill of any registered agent are rejected
with 409, so a user's skill never sits shadowed and unusable.

To mount management APIs without AG-UI, use
`skills.NewHandler(store, skills.WithNamespaceResolver(resolve), skills.WithReservedNames(globals))`.
The resolver receives the authenticated request and returns the allowed namespace;
without one, every route returns 403. `WithReservedNames` returns the global skill
names uploads may not reuse.

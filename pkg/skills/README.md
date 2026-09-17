# Persistent skills

`skills.Store` is a namespace-scoped interface for storing complete skill bundles:
`Put`, `Get`, paginated `List`, and idempotent `Delete`. Implement it for your own
database or service, or use the filesystem and S3 implementations.

A bundle contains `SKILL.md` and supporting files keyed by paths relative to the
skill folder. `SKILL.md` must have YAML frontmatter with an explicit `name` and
`description`. Policies in uploaded frontmatter are ignored. Bundles are limited
to 100 files and 10 MiB of decoded content; absolute and traversing paths are rejected.
The HTTP upload limit is 20 MiB including multipart or base64 JSON overhead.

## Skill sources and agent integration

`skills.NewFilesystemSkillSet(name, directory)` reads ordinary skill folders.
`skills.NewFSSkillSet(name, fsys)` reads embedded or other `fs.FS` content.
Both discover metadata on each listing and enable discovered skills by default.
`skills.NewSkillSet(name, store)` reads uploaded bundles from a persistent store
and defaults to opt-in. All implement the consumer-owned `agents.SkillSet`.

The agent selects skills, builds the prompt metadata, and creates one `read_skill`
tool. Folder discovery, frontmatter parsing, and persistence live in this package.
A request for instructions returns the body without YAML frontmatter; an explicit
`file: "SKILL.md"` returns the original document. Supporting files are read as-is.

The old public `SkillRegistry`, `AsSkillSet`, `SkillTool`, `SkillHint`, and
`NewReadSkillTool` APIs have been removed. Use source constructors directly in
`AgentConfig.Skills`. The root `hastekit.NewFilesystemSkillSet` and
`hastekit.NewFSSkillSet` convenience aliases remain supported.

## Filesystem setup and embedded UI

```go
store, err := skills.NewFileStore("./data/skills")
if err != nil { return err }
source, err := skills.NewSkillSet("library", store)
if err != nil { return err }

agent, err := hastekit.NewAgent(&hastekit.AgentConfig{
    Name: "Assistant",
    LLM: model,
    Skills: []hastekit.SkillSet{source},
    Instruction: hastekit.NewPrompt("Use the relevant skills.",
        prompts.WithResolver(prompts.DefaultResolvers()...)),
})
if err != nil { return err }
registry := hastekit.NewRegistry()
if err := registry.Register(agent); err != nil { return err }
return web.Serve(":8080", registry, agui.WithSkillStore(store))
```

Imports: `pkg/skills`, `pkg/agents/prompts`, `pkg/agui`, `pkg/agui/web`, and the root
SDK as `hastekit`. `model` is your configured provider. Protect the HTTP handler
with your application's authentication and management authorization before
exposing it publicly; `web.Handler` can be wrapped with that middleware.

The composer's **+ → Skills → Manage Skills** action uploads a skill folder or selected
files, lists stored skills, previews SKILL.md as text, downloads resources, and deletes skills with an inline confirmation. Deletion refreshes the agent picker.
The upload button explicitly replaces a same-named skill's entire bundle.
Newly uploaded skills appear in the agent picker when it uses this store adapter.
Saving content does not automatically enable opt-in skills. The library represents
the namespace's stored content; an agent's picker reflects its configured sources
and policies, so a stored skill need not be available to every agent.

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
source, err := skills.NewSkillSet("library", store)
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
This persistent format is separate from `NewFilesystemSkillSet`, which reads an
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

## Namespaces and policies

Management routes use `agui.WithNamespaceResolver`, defaulting to `default`.
Request bodies and query parameters cannot select another namespace. An agent
execution passes `Input.Namespace` explicitly to skill listing and reading, using
`default` when empty. Direct calls use `ListSkills(ctx, namespace, runContext)`
and `ResolveSkill(ctx, namespace, runContext, name, file)`. The agent picker uses
`agent.ListSkills(ctx, namespace, runContext, selection)`. `RunContext` remains
application data; a key named `Namespace` has no special meaning.

To read shared built-ins from a global namespace, configure
`skills.WithNamespaceResolver(func(ctx context.Context, namespace string,
runContext map[string]any) (string, error) { return "global", nil })`. The resolver
receives the explicit namespace and may deliberately override it.

The adapter defaults to `SkillOptIn`. Set a host-controlled default or per-skill
policies when constructing it:

```go
source, err := skills.NewSkillSet("library", store,
    skills.WithDefaultPolicy(agents.SkillEnabled),
    skills.WithPolicies(map[string]agents.SkillPolicy{
        "safety-checklist": agents.SkillRequired,
        "retired-process": agents.SkillBlocked,
    }),
)
```

New runs refresh the catalog. An active run's enabled names and resource allowlist
remain fixed; content reads see the latest saved bundle. Use an application-specific
versioned store if content must remain pinned for an entire run. Deleting a skill
makes later reads fail; it does not erase previously read text from conversation history.
Temporal and Restate use the existing skill-set activity/run-step wrappers.

## HTTP API

Pass `agui.WithSkillStore(store)` to `web.Serve` or `web.Handler` to enable management. Omit it to disable management. The **+ → Skills → Manage Skills** button opens uploads and browsing. The embedded UI mounts these below `/api/agui`:

| Method | Path | Behavior |
| --- | --- | --- |
| GET | `/skills?limit=50&cursor=...` | Metadata page, with opaque `nextCursor` |
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
content, 403 for namespace denial, 404 for missing content, and 413 for size limits.
Backend errors return 500 without exposing storage details. File responses are
attachments with `nosniff`; the UI never executes uploaded scripts or renders
uploaded HTML.

To mount management APIs without AG-UI, use `skills.NewHandler(store, resolver)`.
The resolver receives the authenticated request and returns the allowed namespace.

Skill names have no source prefix. When names collide, the last entry in `AgentConfig.Skills` wins, replacing the description, policy, allowed files, and resolver. Within a source, the last listed entry wins. Selection policies apply after merging. Source names must still be unique for runtime registration.

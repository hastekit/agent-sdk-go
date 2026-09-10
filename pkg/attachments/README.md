# Owned attachments

Store files once, put `file_id: "attachment://..."` in messages, and resolve them to inline data only
inside the model call that needs them. The filesystem implementation is included;
applications can implement `Store` / `UploadStore` for S3 or other storage.

The shared Responses types retain their existing `file_id` fields. Use
`attachments.FileID(ref)` to encode an immutable reference and
`attachments.RefFromFileID(id)` to decode one. The reserved `attachment://`
scheme distinguishes SDK-owned files from ordinary provider file IDs, which
the middleware passes through unchanged. It is an opaque reference, not a URL
that a provider or browser fetches. Browser previews use `attachments.URL(ref)`.

Existing history written with the previous `file_ref: {id, version}` extension
must be converted to `file_id: "attachment://<id>?version=<version>"` when
upgrading; the shared schema no longer has a `file_ref` field.

## Local filesystem setup

```go
import (
    "context"
    "os"

    hastekit "github.com/hastekit/agent-sdk-go"
    "github.com/hastekit/agent-sdk-go/pkg/agents"
    "github.com/hastekit/agent-sdk-go/pkg/agents/history"
    "github.com/hastekit/agent-sdk-go/pkg/agents/middleware"
    "github.com/hastekit/agent-sdk-go/pkg/attachments"
    "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
    "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
    "github.com/hastekit/agent-sdk-go/pkg/utils"
)

// Files are stored under the agent namespace, passed to the store explicitly.
// Inside a run the middleware takes it from the call; outside one, pass the namespace
// the identity check chose.
ctx := context.Background()
store, err := attachments.NewFileStore("./data/attachments", attachments.FileStoreConfig{
    MaxFileBytes: 20 << 20,
})
if err != nil { panic(err) }
defer store.Close() // close at service shutdown, after requests finish

file, err := os.Open("photo.png")
if err != nil { panic(err) }
ref, uploadErr := store.Put(ctx, "tenant-123", attachments.Upload{
    Filename: "photo.png", MediaType: "image/png", Content: file,
})
file.Close()
if uploadErr != nil { panic(uploadErr) }

// One resolver, shared by every agent that reads from this store: its byte
// cache is what keeps a repeated image from being read again per call.
resolver := attachments.NewResolver(store, attachments.Config{
    CacheBytes: 64 << 20,
    MaxFileBytes: 20 << 20,
    MaxConcurrentLoads: 4,
})
client := hastekit.NewLLMClient(providerConfigs)

input := responses.InputMessageUnion{OfInputMessage: &responses.InputMessage{
    Role: constants.RoleUser,
    Content: responses.InputContent{
        {OfInputText: &responses.InputTextContent{Text: "What is in this picture?"}},
        {OfInputImage: &responses.InputImageContent{FileID: utils.Ptr(attachments.FileID(ref)), Detail: "auto"}},
    },
}}

// Any persistence adapter can be used here, including one backed by Postgres.
conversationHistory := hastekit.NewFileHistory("./data/conversations")
agent := agents.NewAgent(&agents.AgentOptions{
    Name: "vision",
    LLM: client.Model("OpenAI/your-vision-model"),
    History: conversationHistory,
    // The middleware is what turns attachments on for this agent: it resolves
    // attachment file_id inputs inside each model call, and stores what tools return.
    Middlewares: []agents.Middleware{middleware.NewAttachmentMiddleware(middleware.AttachmentMiddlewareConfig{
        Store: store,
        Resolver: resolver,
        MaxInlineBytes: 32 << 20,
    })},
})
_, err = agent.ExecuteWithoutTrace(ctx, &agents.AgentInput{
    Namespace: "tenant-123", ThreadID: "thread-1",
    Message: history.Message{ID: "message-1", SenderID: "user-1",
        Messages: []responses.InputMessageUnion{input}},
})
if err != nil { panic(err) }
```

`providerConfigs` is your normal provider configuration. The example is a setup
recipe, not a complete executable. `NewFileHistory` is the existing convenience
constructor; use `history.NewFileConversationPersistence` directly when you need
explicit error/close handling.

For documents use `responses.InputFileContent{FileID: utils.Ptr(attachments.FileID(ref))}`. Metadata supplies
the original filename and MIME type; model/provider document support still
applies. The same reference forms work inside multimodal tool-result content.
Tools should upload their files before returning a reference.

All callers in a namespace can read its attachments. Wrap the store or
implement your own to enforce finer per-file ACLs.

## Storage and transport contracts

- `Store.Lookup(ctx, namespace, ref)` authorizes every access, including cache
  hits, and returns authoritative `Descriptor` metadata. The namespace is the
  agent namespace and must isolate storage and tenants; key/version must
  identify immutable bytes.
- `Store.Open(ctx, descriptor)` opens that immutable content using backend
  credentials. It must honor cancellation. No signed URL is needed.
- `UploadStore` adds `Put(ctx, namespace, Upload) (Ref, error)`. Uploads happen
  before agent, broker, or workflow submission. A remote store can implement
  both interfaces.
- The local store uses random immutable IDs, SHA-256 versions, hashed namespace
  directories, private file permissions, and `os.Root` path confinement. It
  writes content and metadata separately and returns the reference after both
  are flushed. Image MIME declarations are checked against file signatures.
- Treat the store directory as SDK-owned. Do not edit the files behind an ID.
  Back up the entire directory together with conversation history. Interrupted
  processes can leave unreferenced upload files; retention/garbage collection is
  application-managed in this version.

Resolution is the middleware's `WrapModelCall`. It wraps the model call inside the
step that makes it — the agent loop locally, the LLM activity under Temporal,
the LLM run step under Restate — after the request has crossed into that step,
and hands the provider a transient copy carrying inline data. The loop,
conversation history, traces, and the durable journal keep the references.
Adding the middleware to an agent turns attachment support on for that agent;
removing it turns it off, and the LLM client is not involved either way. OpenAI
gets data URLs; Anthropic/Gemini/Bedrock adapters translate those into their
inline representation. A request carrying no references passes through
untouched.

The LLM client and its middleware chain never touch attachments: retry,
fallback and tracing all see the request the middleware produced, and the client
sends exactly what it is handed. A caller dispatching Responses requests
outside an agent — a direct `client.Model(...)` call, or an external gateway
server with access to the store — prepares them itself with
`middleware.PrepareAttachments(ctx, namespace, ...)`, which is the same function the middleware uses.
Without a middleware or that preparation, the SDK does not resolve `file_id: "attachment://..."`, and
the request continues through the existing provider adapter without an SDK
validation step. A provider may reject the unresolved ID or its adapter may
not support carrying it.

## Cache and limits

The resolver is service-lived and concurrency-safe. A byte-bounded LRU retains
raw bytes. Concurrent misses for an immutable object share one load. Each caller
is independently authorized. Cancelling a waiter does not cancel another
waiter's load; shared reads have a timeout (30 seconds by default). Set
`middleware.AttachmentMiddlewareConfig.Resolver` to share one resolver — and its cache —
across agents; a middleware given only a `Store` builds a private resolver over it.

Defaults: 64 MiB retained cache, 20 MiB per file, four simultaneous storage loads,
and 32 MiB aggregate encoded attachment content per model request. Negative
`CacheBytes` disables retention. Configure the aggregate limit using
`middleware.AttachmentMiddlewareConfig.MaxInlineBytes`. Encoded limits count repeated message
occurrences, even if the storage read is deduplicated. Provider request, MIME,
image dimension, and model limits still apply. These are not a global limit on
all simultaneous LLM request allocations.

Cache eviction, worker restarts, and different workers can cause another read.
The filesystem backend reads local files; an S3 adapter would download on a
miss. Encoding happens per request. Inline bytes are still uploaded to the
provider on every call that includes the image. Provider upload-ID caching and
a shared/distributed content cache are not part of this version.

## Durable runtimes and persistence

The middleware takes the namespace from the call it wraps: `ToolCall.Namespace` on
the way back from a tool, `ModelCall.Namespace` on the way to the model. Under
Temporal and Restate both travel with the call into the activity or step, so
nothing has to be registered on a client or worker. Resolution runs inside the
LLM activity or step, on the real middleware, so the journal contains references
only. Protect durable ingress: workflow input is an internal server contract,
not a source of authenticated identity. Application uploads should normally
happen at ingress before starting the workflow.

The caller's contract is: upload first, then send a `file_id: "attachment://..."`. History stores
a message as it arrives, and the middleware resolves references for the model, so a
message that carries a reference is one whose bytes never enter a queue, a
journal, or a transcript. Keep binary payloads out of run metadata and text
fields for the same reason.

This version handles input images/documents and referenced tool results. The
optional tool middleware below externalizes structured inline function-tool results.
Provider-generated image streams are not automatically externalized. Tools must upload
and return references themselves when bytes must not cross any durable boundary.
Applications using native image-generation streaming need a separate output
storage adapter before persisting those events.

Browser downloads are separate from LLM transport: the HTTP API below authorizes
each fetch through the store and streams the file without exposing storage paths.

## Browser upload and chat

The embedded chat enables its file picker when you pass
`agui.WithAttachmentStore(store)` to `web.Handler` or `web.Serve`. Use that same
store in the agent's attachment middleware. A complete runnable example is in
[`samples/attachments`](../../samples/attachments/main.go):

```sh
OPENAI_API_KEY=... go run ./samples/attachments
# Open http://localhost:8080
```

The API exposes:

- `POST /attachments/`: multipart form with one `file` field. Returns HTTP 201
  and `{file_id, url, filename, mediaType, size}`.
- `GET /attachments/{id}?version=...`: authorizes through `Store.Lookup`, then
  streams `Store.Open`. Images display inline; PDFs download as documents.

`attachments.NewHTTPHandler(store, maxBytes, namespaceOf)` can also be mounted
independently; `namespaceOf` reports a request's namespace from whatever
authenticated it.
The default upload limit is 20 MiB; `agui.WithAttachmentUploadLimit` can lower it.
The embedded picker has a 20 MiB client limit. Supported browser uploads are PNG,
JPEG, GIF, WebP, and PDF, verified from their signatures. File signature checks
identify format; they do not perform malware scanning or validate all document
structure. A custom UploadStore can add processing before returning a reference.

The browser uploads first and sends AG-UI image/document parts whose `source`
is `{type: "url", value: "/attachments/<id>?version=..."}`. These are owned API
URLs, not arbitrary fetch targets. AG-UI validates ownership and format, pins
an immutable version, and converts them to SDK `file_id: "attachment://..."` inputs **before**
starting or enqueueing the turn. Client-provided MIME types and filenames are
replaced by storage metadata. Inline data and external URLs in multipart input
are rejected. Plain text messages remain compatible.

History reload converts references back to browser URLs. Live multimodal input
is announced as a CUSTOM `input_message` event containing an AG-UI Message;
the embedded client upserts it by message ID, including when rejoining a run.
External clients should handle that event to display incoming multimodal turns.
The agent's attachment middleware then resolves the original references inside the
model call, through its byte cache, using image/file-specific provider fields.
Neither browser URLs nor resolved bytes are written back to conversation history.

These routes do not install authentication or infer a tenant from a file ID.
Mount the entire HTTP handler behind your existing authentication. The AG-UI
handler serves the attachment routes under its namespace, the same one every
run it starts reads them under, so uploads and runs agree. The local example
uses the default namespace and binds to localhost. Browser fetches use same-origin credentials; providers
do not fetch these URLs. Replacing FileStore with S3 requires implementing
UploadStore, not changing the API or UI. IDs must be single URL path segments;
references should carry a version when an ID can refer to changing content.

## Externalizing tool results

The same middleware that resolves references for the model stores what tools return.
Register it on each agent that sends or returns files:

```go
Middlewares: []agents.Middleware{
    middleware.NewAttachmentMiddleware(middleware.AttachmentMiddlewareConfig{
        Store: store,
        Resolver: resolver, // optional; built over Store when omitted
        MaxFileBytes: 20 << 20,
    }),
},
```

Tools should return `FunctionCallOutputMessage.Output.OfList` containing
`OfInputImage` with an `ImageURL` data URI, or `OfInputFile` with `FileData`
(base64 data URI or raw base64) and an optional `FileName`. Raw base64 files have
their MIME type inferred from the bytes. The middleware decodes each attachment,
uploads through `UploadStore`, and replaces its transport fields with an `attachment://...` value in `file_id`.
Existing references, text parts, call IDs, state updates, and task metadata are
preserved. The original result is not mutated. Repeated identical attachments
with the same filename within one result share an upload.

The MCP adapter preserves text/image/embedded-resource content in the same
structured format, so MCP images and file blobs reach this middleware too. Single
text-only MCP results retain the existing string shape. Resource links are not
fetched; embedded resources must contain bytes or text.

Register this middleware after other transformations that produce media. Ordinary
after-tool middlewares receive the transformed result. Upload or decoding failures abort the call rather than allow
inline output into conversation history. Uploads are not a transaction: if a
later attachment fails, earlier successful uploads can remain unreferenced.
Do not put attachment bytes into arbitrary string/JSON output, state updates,
interrupts, or task payloads: those fields are not scanned. Remote URLs,
provider file IDs, and local paths must be explicitly loaded/uploaded by the
tool or an application-specific middleware. The standard middleware does not fetch them.
Provider-native image-generation results are model output, not function tool
results, and do not pass through this middleware.

The same middleware resolves these new refs on subsequent model calls, in its
`WrapModelCall`, and the persisted tool results contain refs. `WrapToolCall`
runs inside the Temporal activity or Restate run callback, around the tool,
before its result is journaled. This applies to ordinary and MCP tool calls,
background-task results, and results another middleware's wrap answers with, which
flow back out through the wraps outside it. It is never registered as a
separate durable step.

Middlewares nest in registration order, the first outermost, so a result flows from
the tool through the last middleware's wrap to the first's. Register the attachment
middleware first, so a result another middleware's wrap adds media to still passes through
it. A wrap can run multiple times: once per result-producing boundary, and
again if that boundary retries. Use immutable, retry-safe uploads. FileStore
creates fresh immutable objects, so retries may leave unused uploads; it does
not promise cross-attempt deduplication. The middleware's own errors retain middleware-abort
semantics and return no raw result; a tool's error passes back out as the
tool's. Background failures are reported as task failures. Progress events,
arbitrary metadata and native model outputs remain outside this contract.

All workers that may resume a task need access to the same durable attachment
store. A machine-local store is suitable for a single worker;
use shared storage for distributed workers. Already journaled payloads are not
rewritten by this change.

`ToolCallMiddleware` (and therefore `Middleware`) requires `WrapToolCall(next)`, and
`ModelCallMiddleware` requires `WrapModelCall(next)`. Embed `agents.NoopMiddleware`
and override what you need; it provides defaults for model, tool, history and
prompt operations. Custom runtime adapters must run the wraps inside their
own execution boundary: `agents.ExecuteToolCallWithMiddleware` around a tool,
`agents.WrapBackgroundTool` to bind middleware around the real tool's
`AwaitTask` before registering its durable wait, and `agents.ExecuteModelCallWithMiddleware`
around the provider. These share the SDK's nesting and failure behavior.

Workflow-side agents leave `Middlewares` empty: real middleware is bound to the
worker's tool and model operations. Do not turn either wrap into another
activity or run step: its input would then journal the raw bytes.

Background waits use the same tool middleware around `AwaitTask`, so a wrap can
short-circuit the wait or recover its error. The local agent binds this wrapper
when dispatching the task; Temporal binds it to the wait activity and Restate
to a registered wait operation. The supervisor only waits and delivers. A
handoff changes neither the tool's middleware nor the owner of the conversation.

Migration: Temporal MCP activity names now include the agent name
(`agent_server_ExecuteMCPToolActivity` and `agent_server_ListMCPToolsActivity`).
Restate background inputs now carry `tool_key`, supplied by the tool proxy,
instead of `tool_agent_name` and `tool_name`. `BackgroundTaskRef.ToolAgentName`
has been removed. Drain existing durable runs and background tasks on the old
workers, or use a separately versioned deployment for new runs; old histories
and queued task inputs are not automatically migrated to these routing names.

# Sandbox execution

All backends implement one contract:

```
Tool → Acquire(run.State)
         reference present → Provider.Connect
         reference absent  → Provider.Create
      → Exec / Files
      → StateUpdates → next tool call and persisted run.State

Backend: Docker/Kubernetes → HasteKit daemon
         E2B               → envd
         Daytona           → toolbox
```

`Provider` owns infrastructure, readiness, routing, and authentication.
`run.State[sandbox.StateKey]` is the authoritative resource reference. There is
no separate binding store, provisioning mutex, or tool preparation phase.
Middleware and approval run before the tool can provision compute.

## Usage

Pass an in-process backend directly to tools:

```go
bash := tools.NewBashTool(provider, "analysis", nil)
```

`Acquire(ctx, provider, state, request)` reconnects to a saved reference, or creates
when the state has no reference. It returns the sandbox and state updates. Built-in
tools carry those updates in their results, including ordinary command/file
failures after acquisition. Cancellation or a crash before the result is persisted
can still orphan compute. No automatic command retries are introduced.

All tool calls execute sequentially. Local, Temporal, and Restate executors pass
successful response state updates to the next call in the same batch. State must
also be persisted and restored between runs. It contains the reference, profile,
and session, excluding credentials and environment values. Corrupt state, a scope
mismatch, or a missing saved resource fails instead of silently provisioning again.
Environment and TTL are creation settings; reconnect does not reconfigure compute.

Concurrent runs of the same session are unsupported, including runs in different
threads or workers. Separate threads do not share compute automatically: they must
receive the same state explicitly if sequential reuse is intended.

Docker, Kubernetes, and Daytona use stable session resource names. On a provider
name conflict they verify the creation-request fingerprint and image/snapshot,
then reconnect and wait for readiness. A mismatch fails with `ErrConflict`; a
conflicting resource is never deleted by the losing creator. These provider
checks do not make concurrent runs safe. E2B has no equivalent creation identity
in this adapter: HTTP 409 is returned as `ErrConflict`, without guessing a resource
from metadata or retrying creation.

`NewBashTool` calls `Exec` with `[]string{"bash", "-c", code}`. Working directory
is explicit in the optional `workdir` argument. `cd` affects that invocation only.
The configured image/template must provide Bash. FileSystem accepts binary readers
and streams; for example, `sb.Files().Write(ctx, "uploads/report.pdf", reader)`.

Provider imports are under `github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/providers`.
E2B profiles select a template, Daytona profiles select a snapshot, and SDK
Docker/Kubernetes profiles configure native containers and pods. Changing providers
does not change tools.

Standalone SDK implementations:

- [`providers/docker`](providers/docker/README.md): official Docker Engine client,
  configurable mounts/networks/resources, image pulling and daemon port routing.
- [`providers/k8s`](providers/k8s/README.md): client-go, native pod templates,
  application-managed PVC mounts and configurable daemon routing.

Both accept injected clients and session-specific configuration callbacks. They
require an image serving the daemon protocol and do not depend on gateway paths
or service configuration. Docker references retain immutable container IDs;
Kubernetes references retain namespace, pod name and UID.

## Gateway integration

The gateway HTTP client and routes live in `pkg/hastekitgateway`, not in the
sandbox package. `hastekitgateway.Config.NewSandboxClient()` returns its
gateway-specific `SandboxClient`, which also implements `sandbox.Provider`.
The gateway server mounts `hastekitgateway.NewSandboxHandler`. Custom backends
implement `sandbox.Provider` in process; there is no generic HTTP provider API.

## Gateway configuration

Set `SANDBOX_ENABLED=true` and choose `SANDBOX_PROVIDER`:

| Provider | Required configuration |
| --- | --- |
| `docker` (default) | `SANDBOX_DEFAULT_IMAGE` |
| `k8s` | `SANDBOX_DEFAULT_IMAGE`, in-cluster Kubernetes credentials |
| `e2b` | `E2B_API_KEY`, `E2B_TEMPLATE_ID` |
| `daytona` | `DAYTONA_API_KEY`, `DAYTONA_SNAPSHOT` |

`SANDBOX_PROFILE` names the configured profile. If omitted it defaults to
`SANDBOX_DEFAULT_IMAGE`, or `default` for cloud providers. Agent sandbox config
uses `profile`, and leaving it empty selects the default. The built-in factory
registers one profile; applications can supply additional typed profiles when
constructing a provider. An unknown profile is an error.

`E2B_API_URL` and `DAYTONA_API_URL` override control-plane endpoints. Sandbox
provisioning has no Redis dependency. HTTP creation calls the backend directly;
execution and file routes use explicit provider/ID references.

## Execution and files

Commands use literal arguments. Shell interpretation occurs only when the caller
explicitly launches a shell. Nonzero exit codes are results, not transport errors.
Zero timeout selects 60 seconds. Context cancellation stops waiting; remote process
termination depends on the provider. Do not retry commands blindly.

| Behavior | HasteKit daemon | E2B | Daytona |
| --- | --- | --- | --- |
| Exec transport | Native argv over HTTP | Native argv through envd Connect RPC | Toolbox command API with literal argument quoting |
| Output | Separate streams | Separate streams | Combined in stdout, `OutputCombined=true` |
| Timeout | Kills process group, exit 124 | Cancels stream and attempts native PID kill | Sent to toolbox in seconds |
| Files | Binary HTTP stream | Native multipart upload and binary download | Native raw upload and binary download |
| Lifetime | Configured daemon idle timeout | TTL seconds | Wall-clock TTL rounded up to minutes |

HasteKit tracks active requests so a long command or upload does not trigger its
idle shutdown. E2B and HasteKit cap output at 4 MiB per stream and flag truncation.
Daytona JSON responses are limited to 16 MiB; oversized responses fail decoding.
The model-facing read tool allows at most 4 MiB of UTF-8 text; `FileSystem` transfers
arbitrary binary data. HasteKit writes publish atomically and confine paths with
`os.Root`, including symlink traversal.

Compute is created during the first sandbox tool execution, not when an attachment is staged in
application storage. Docker/Kubernetes can mount the existing session workspace.
Cloud providers need supported provider storage or a copy through `Files().Write`;
an application S3/EFS folder does not automatically become a cloud sandbox mount.
The gateway's coding tool still requires mounted worktrees and service endpoint
access; that application-specific tool is not a portable replacement for Exec.

## Conversation lifetime and migration

Missing compute is an error, not an implicit replacement. To reset a sandbox,
call `Delete(reference)` and explicitly clear `run.State[sandbox.StateKey]`.
The next acquisition can create fresh compute. E2B reconnect does not extend TTL
and rejects paused sandboxes. Daytona can start stopped or archived sandboxes.

State is persisted at the normal run/tool-result boundaries, not transactionally
with provider creation. A crash or cancellation between these operations can
orphan compute. Provider conflict recovery reduces duplicates where supported,
but does not provide exactly-once creation or execution.

This is a breaking migration: rebuild the daemon image and deploy SDK/gateway
together. The old lifecycle endpoints, text file protocol, `/exec/bash`, legacy
manager, and shell result types are removed. Gateway routes are now
`POST /api/sandbox/` for creation and `GET`/`DELETE /api/sandbox/{provider}/{id}`
for reconnect/deletion, with `/v2/exec` and `/v2/files` suffixes for runtime
operations. Saved agent configs must replace `docker_image` with a configured
`profile`. Drain old compute resources; new references do not adopt old
session-named containers. Mounted session data remains in its existing location.

Provider tests use mocked official protocols; live E2B/Daytona accounts and
Docker/Kubernetes deployments are needed for deployment integration tests.

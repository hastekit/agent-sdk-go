# Sandbox execution

The SDK includes a reusable Docker base image. See the
[image guide](../../../deployments/sandbox/README.md) for building, configuring,
extending, and publishing it.

All backends implement one contract:

```
Tool → Acquire(run.State)
         reference present → Provider.Connect
         reference absent  → Provider.Create
      → Exec
      → StateUpdates → next tool call and persisted run.State

Backend: Docker     → Engine exec
         Kubernetes → API server exec
         E2B        → envd
         Daytona    → toolbox
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
The configured image/template must provide Bash.

A sandbox only runs commands. The file tools use `sandbox.ReadFile`, `WriteFile` and
`RemoveFile`, which are built on `Exec` and so work on any backend. They need a POSIX
`sh` with `base64`, `cat`, `mkdir`, `mv`, `readlink`, `rm`, `rmdir`, `tr` and `wc`.
Paths are confined to the sandbox's default working directory, including through
symlinks, which scopes these helpers but not a shell. Content is base64 inside each
command, since arguments cannot hold NUL and JSON transports rewrite invalid UTF-8.
`ReadFile` returns at most 3 MB. `WriteFile` publishes atomically and takes one
command per 48 KiB, so it suits what a model writes, not large files: mount those,
or have a command fetch them from inside the sandbox.

Provider imports are under `github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/providers`.
E2B profiles select a template, Daytona profiles select a snapshot, and SDK
Docker/Kubernetes profiles configure native containers and pods. Changing providers
does not change tools.

Standalone SDK implementations:

- [`providers/docker`](providers/docker/README.md): official Docker Engine client,
  configurable mounts/networks/resources, image pulling, and commands and files
  over the Engine's exec API with nothing of ours in the container.
- [`providers/k8s`](providers/k8s/README.md): client-go, native pod templates,
  application-managed PVC mounts, and commands and files over the API server's
  exec subresource with nothing of ours in the pod.

Both accept injected clients and session-specific configuration callbacks, and
do not depend on gateway paths or service configuration. Images need a POSIX shell
and common tools, and a profile's `StartupScript` runs setup when a sandbox starts;
E2B templates and Daytona snapshots have their own start commands for that. Docker
references retain immutable container IDs;
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

| Behavior | Docker | Kubernetes | E2B | Daytona |
| --- | --- | --- | --- | --- |
| Exec transport | Native argv through Engine exec | Native argv through API server exec, sent over stdin | Native argv through envd Connect RPC | Toolbox command API with literal argument quoting |
| Output | Separate streams | Separate streams | Separate streams | Combined in stdout, `OutputCombined=true` |
| Timeout | Kills process group from inside the container, exit 124 | Kills process group from inside the container, exit 124 | Cancels stream and attempts native PID kill | Sent to toolbox in seconds |
| Lifetime | Stopped by the provider after `IdleTimeout`, and by itself after `MaxLifetime` | Deleted by the provider after `IdleTimeout`, and by Kubernetes after `MaxLifetime` | Pauses TTL seconds after creation or the latest reconnect | Deleted after a wall-clock TTL rounded up to minutes |

Neither idle sweep touches a sandbox while a command is running. Docker, Kubernetes
and E2B cap output at 4 MiB per stream and flag truncation. Docker and Kubernetes
kill a command that produces more than 64 MiB in total, with exit 137.
Daytona JSON responses are limited to 16 MiB; oversized responses fail decoding.
The model-facing read tool allows at most 3 MB of UTF-8 text.

Compute is created during the first sandbox tool execution, not when an attachment is staged in
application storage. Docker/Kubernetes can mount the existing session workspace.
Cloud providers need supported provider storage or a command that fetches the file;
an application S3/EFS folder does not automatically become a cloud sandbox mount.
The gateway's coding tool still requires mounted worktrees and service endpoint
access; that application-specific tool is not a portable replacement for Exec.

## Conversation lifetime and migration

Missing compute is an error, not an implicit replacement. To reset a sandbox,
call `Delete(reference)` and explicitly clear `run.State[sandbox.StateKey]`.
The next acquisition can create fresh compute. E2B sandboxes pause when their TTL
runs out, keeping files and processes, and reconnecting resumes a paused sandbox
or extends a running one for another TTL. E2B keeps paused sandboxes until they
are deleted: call the E2B provider's `DeletePaused` periodically with your
retention period. Docker stops idle containers and reconnecting starts them again,
keeping files and installed packages but not processes. Pods cannot be stopped, so
Kubernetes deletes idle ones, keeping only what is on a volume, and the session's
next sandbox call fails until its state is cleared. Daytona can start stopped or
archived sandboxes; a Daytona TTL deletes the sandbox, so leave it unset for
sessions that should resume.

State is persisted at the normal run/tool-result boundaries, not transactionally
with provider creation. A crash or cancellation between these operations can
orphan compute. Provider conflict recovery reduces duplicates where supported,
but does not provide exactly-once creation or execution.

This is a breaking migration: rebuild the image and deploy SDK/gateway together.
The daemon, its port and its idle timeout are gone: any image with a shell works,
and `pods/exec` access replaces routing to pods. Pods and containers created with
the daemon keep working over exec until they end. The old lifecycle endpoints, text
file protocol, `/exec/bash`, legacy manager, and shell result types are removed.
Gateway routes are now
`POST /api/sandbox/` for creation and `GET`/`DELETE /api/sandbox/{provider}/{id}`
for reconnect/deletion, with a `/v2/exec` suffix for runtime
operations. Saved agent configs must replace `docker_image` with a configured
`profile`. Drain old compute resources; new references do not adopt old
session-named containers. Mounted session data remains in its existing location.

Provider tests use mocked official protocols; live E2B/Daytona accounts and
Docker/Kubernetes deployments are needed for deployment integration tests.

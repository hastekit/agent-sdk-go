# Docker provider

This provider implements `sandbox.Provider` directly in the SDK using the official
Docker Engine Go client. Container lifecycle and commands both go through the
Engine API. Nothing of ours runs inside the container and it opens no
port, so other containers have nothing to reach and networking can be disabled.

The image must provide a POSIX `sh` with `cat` and `sleep`, and the file tools also
need `base64`, `mkdir`, `mv`, `readlink`, `rm`, `rmdir`, `tr` and `wc`. `sync` and
`touch` are used when present. Both GNU and BusyBox tools work. Distroless images do not. The working directory defaults
to `/workspace`, and the default entrypoint only keeps the container running, with
`HostConfig.Init` enabled to reap processes. Override the native container
configuration for a different entrypoint, which has to keep running too. The
provider does not build the image.

```go
import (
    "github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/providers/docker"
    "github.com/hastekit/agent-sdk-go/pkg/agents/tools"
    "github.com/moby/moby/api/types/container"
)

provider, err := docker.New(docker.Config{
    Profiles: map[string]docker.Profile{
        "analysis": {
            Config: container.Config{Image: "your-registry/sandbox-base:tag"},
        },
    },
})
if err != nil { return err }
defer provider.Close()
bash := tools.NewBashTool(provider, "analysis", nil)
_ = bash // add to the agent's tools
```

The default Engine client uses `DOCKER_HOST`, `DOCKER_TLS_VERIFY`, and
`DOCKER_CERT_PATH`, with API version negotiation. Inject `Config.Client` to control
Engine connections explicitly. An injected client remains caller-owned.

Images are pulled if missing by default. `PullAlways` and `PullNever` are also
available; `Profile.RegistryAuth` accepts Docker's encoded registry authentication.
Pull stream errors fail provisioning before a container is created.

## Storage and native configuration

`Profile.HostConfig` supports bind mounts, Docker volumes, resources, capabilities,
and native security options. `Profile.NetworkingConfig` configures networks.
The application owns mounted storage; deleting a sandbox does not remove volumes.
For session-specific mounts, use `Config.Configure`:

```go
Configure: func(ctx context.Context, req sandbox.CreateRequest, opts *client.ContainerCreateOptions) error {
    // Resolve an application-owned path. Validate identifiers before using
    // external input as a host path.
    source, err := uploadDirectory(req.Session)
    if err != nil { return err }
    opts.HostConfig.Mounts = append(opts.HostConfig.Mounts, mount.Mount{
        Type: mount.TypeBind, Source: source,
        Target: "/workspace/uploads", ReadOnly: true,
    })
    return nil
},
```

The callback receives an isolated copy. It must produce stable configuration for
the same creation request. The provider assigns the resource name and ownership
labels after customization. `CreateRequest.Env` overrides profile environment;
`SANDBOX_ROOT` is set to the working directory.

## Commands

Commands run through the Engine's exec API, with literal arguments and separate
stdout and stderr. They run under a small shell wrapper that kills the command's
process group when its timeout passes or when the caller goes away, even if the
caller crashed. A background job survives the command that started it. Output past
4 MiB per stream is discarded, and a command that produces more than 64 MiB in total
is killed with exit 137.

## Startup script

`Profile.StartupScript` runs in `sh` each time a container starts, in the working
directory and with the container's environment. That includes being started again
after going idle, when every process is gone, so it is the place to start a service,
and it has to be safe to run again. Something it starts in the background keeps
running, as long as it does not hold the script's output open.

It shares `ReadyTimeout`, so raise that for a slow script. If it fails, `Create`
removes the new container and `Connect` stops the restarted one, and the error
includes what the script printed. It is stored in a container label, so keep secrets
out of it. A different script makes `Create` report a conflict with an existing
container for that session, and the existing one keeps the script it was created with.

For example, to put skill executables on the `PATH`:

```go
Profile{
    // ...
    StartupScript: `mkdir -p /usr/local/bin
for file in /skills/*/bin/*; do
    [ -f "$file" ] && ln -s "$file" /usr/local/bin/ 2>/dev/null
done
exit 0`,
}
```

## Service ports

Only services the sandbox runs itself need a route. Publish their ports through
`HostConfig.PortBindings`; `PublishedHost` is the host reachable from the SDK,
which defaults to 127.0.0.1. `Config.Endpoint(ctx, inspectedContainer, port)`
overrides routing for private networks, reverse proxies, or TLS endpoints. Both
apply to ports requested through `sandbox.EndpointResolver`.

## Lifecycle

- Names are stable per namespace/session. A create conflict reconnects only when
  ownership and the complete effective creation configuration fingerprint match.
- References store immutable container IDs. `Connect` never creates replacements.
- `Connect` starts a stopped container. Paused, dead, and removing containers
  fail explicitly.
- Failed initial startup cleans up only the container created by that call.
  Failed conflict recovery never removes the existing container.
- `Delete` force-removes the referenced container and is idempotent when missing.
- `ReadyTimeout` bounds pulling and startup. Provider TTL is unsupported; use
  `IdleTimeout` and `MaxLifetime`.

Use normal `run.State` reference persistence. Concurrent runs of the same session
remain unsupported; provider conflict recovery does not serialize tool execution.

## Idle containers

The provider stops running containers that have had no activity for `IdleTimeout`,
15 minutes by default. `Close` ends this, and a negative `IdleTimeout` turns it off,
leaving `StopIdle(ctx, idle)` for callers who want to run it themselves.

- Activity is any command through a provider, recorded in
  `/tmp/.hastekit-activity` inside the container. It is read back from there, so
  any number of providers can sweep one engine without coordinating; the shortest
  `IdleTimeout` among them wins. Without a writable `/tmp`, activity counts only
  from when the container last started.
- A running command always keeps its container alive. Background processes and
  traffic to service ports do not count as activity, so call `KeepAlive(ctx)` on the
  sandbox while they are what matters.
- Stopping keeps the container's files and installed packages and loses its
  processes. The next `Connect` starts it again.
- `MaxLifetime`, 24 hours by default, is the backstop for when no provider is running.
  The default entrypoint exits after that long, even if a command is running, and
  `Connect` starts the container again. A custom entrypoint decides this for itself.
- Stopped containers stay until `Delete`; nothing removes them automatically.

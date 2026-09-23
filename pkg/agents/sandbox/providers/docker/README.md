# Docker provider

This provider implements `sandbox.Provider` directly in the SDK using the official
Docker Engine Go client. Container lifecycle uses the Engine API; execution and
file transfers use the sandbox daemon's HTTP protocol.

Supply an image containing a daemon that serves `/health`, `/v2/exec`, and
`/v2/files`. By default the executable is `sandbox-daemon`, with `/workspace` as
its working directory and port 8080. Override the native container configuration
for images with a different entrypoint. The provider does not build the image.

```go
import (
    "github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/providers/docker"
    "github.com/hastekit/agent-sdk-go/pkg/agents/tools"
    "github.com/moby/moby/api/types/container"
)

provider, err := docker.New(docker.Config{
    Profiles: map[string]docker.Profile{
        "analysis": {
            Config: container.Config{Image: "your-registry/sandbox-daemon:tag"},
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
`HTTPClient` separately configures connections to the daemon.

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
`SANDBOX_ROOT` and `SANDBOX_PORT` are initially set from daemon configuration.

## Networking

By default the daemon port is published to an ephemeral host port on 127.0.0.1,
which works with a local engine and Docker Desktop. `PublishAddress` controls the
bind address; `DaemonHost` is the host reachable from the SDK. For a remote engine,
configure both appropriately rather than relying on local loopback.

`Config.Endpoint(ctx, inspectedContainer, port)` overrides routing for private
networks, reverse proxies, or TLS endpoints. With this callback set, the provider
does not automatically publish the daemon port. It also resolves ports requested
through `sandbox.EndpointResolver`. With default routing, additional service
ports must be explicitly published through `HostConfig.PortBindings`.

## Lifecycle

- Names are stable per namespace/session. A create conflict reconnects only when
  ownership and the complete effective creation configuration fingerprint match.
- References store immutable container IDs. `Connect` never creates replacements.
- `Connect` starts a stopped container and waits for daemon readiness. Paused,
  dead, and removing containers fail explicitly.
- Failed initial startup cleans up only the container created by that call.
  Failed conflict recovery never removes the existing container.
- `Delete` force-removes the referenced container and is idempotent when missing.
- `ReadyTimeout` bounds pulling and readiness. Provider TTL is unsupported;
  configure daemon lifetime through the image/environment instead.

Use normal `run.State` reference persistence. Concurrent runs of the same session
remain unsupported; provider conflict recovery does not serialize tool execution.

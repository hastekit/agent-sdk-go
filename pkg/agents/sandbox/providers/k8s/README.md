# Kubernetes provider

This provider implements `sandbox.Provider` directly in the SDK using `client-go`.
It provisions pods running the sandbox daemon and executes commands/transfers
files over the daemon's HTTP API. It does not depend on the gateway or kubectl.

```go
import (
    "github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/providers/k8s"
    "github.com/hastekit/agent-sdk-go/pkg/agents/tools"
    corev1 "k8s.io/api/core/v1"
)

provider, err := k8s.New(k8s.Config{
    Namespace: "sandboxes",
    Profiles: map[string]k8s.Profile{
        "analysis": {
            Template: corev1.PodTemplateSpec{
                Spec: corev1.PodSpec{
                    Containers: []corev1.Container{{
                        Name: "sandbox",
                        Image: "your-registry/sandbox-daemon:tag",
                    }},
                },
            },
        },
    },
})
if err != nil { return err }
bash := tools.NewBashTool(provider, "analysis", nil)
_ = bash // add to the agent's tools
```

The image must contain `sandbox-daemon` serving `/health`, `/v2/exec`, and
`/v2/files`. Configure the container `Command`/`Args` for a different entrypoint.
The default daemon container name is `sandbox`; `Profile.DaemonContainer` selects
another. The provider defaults its working directory to `/workspace`, port to
8080, and pod restart policy to `Never`. It sets `SANDBOX_ROOT` and `SANDBOX_PORT`
on the daemon container. Request environment overrides explicit template entries.

## API-server connection

Provide `Config.Client` (a `CoreV1Interface`) or `RESTConfig` explicitly, or use
in-cluster credentials. Outside a cluster, default kubeconfig loading is used;
`Config.Kubeconfig` can select a particular file. The namespace must already exist.
The provisioning identity needs `create`, `get`, and `delete` access to pods in
that namespace. The provider does not need PVC creation/deletion privileges.

## Storage, resources, and scheduling

The native `PodTemplateSpec` configures volumes, mounts, resources, image pull
secrets, service accounts, security contexts, scheduling and sidecars. For example,
mount an existing PVC and set `VolumeMount.SubPath` to an application-managed
session directory. The provider never creates or deletes application storage.

`Config.Configure(ctx, req, pod)` runs on a deep copy of the profile and can set
session-specific PVCs, subpaths or scheduling. Resolve and validate identifiers
in application code. The callback must produce stable configuration for reuse.
The provider assigns the pod name, namespace and ownership metadata afterwards.

## Daemon networking

The default route is `http://<pod IP>:<DaemonPort>`; the SDK process must be able to
reach pod IPs. `Config.Endpoint(ctx, pod, port)` can return a reachable service,
proxy, or TLS URL instead. `HTTPClient` configures the daemon transport separately
from Kubernetes authentication. No implicit port forwarding is launched.

The same callback resolves other service ports through `sandbox.EndpointResolver`.

## Lifecycle

- A stable name scopes compute to a namespace/session. Kubernetes `AlreadyExists`
  triggers lookup, matching ownership/configuration checks, and readiness wait.
- References contain the Kubernetes namespace, pod name and immutable UID.
  `Connect` rejects a replacement pod even if its name is identical.
- `Delete` uses a UID precondition so an old reference cannot delete replacement
  compute. Missing pods are treated as already deleted.
- Pending pods are awaited; terminating or completed/failed pods are rejected.
  Reconnecting never replaces missing or failed compute.
- Failed initial startup cleans up only the pod created by that call, using its
  UID. Failed conflict recovery leaves existing compute intact.
- `ReadyTimeout` defaults to two minutes. Provider TTL is unsupported; configure
  daemon lifetime through the image/environment instead.

References live in `run.State`. Concurrent runs of one session remain unsupported,
and a crash before a reference is persisted can still leave orphan compute.

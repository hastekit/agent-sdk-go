# Kubernetes provider

This provider implements `sandbox.Provider` directly in the SDK using `client-go`.
It provisions pods and runs commands through the API server's exec subresource. It does not depend on the gateway or kubectl. Nothing of ours
runs inside the pod beyond a shell keeping it alive, and it listens on no port, so
other pods have nothing to reach and the SDK needs no route to pod IPs.

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
                        Image: "your-registry/sandbox-base:tag",
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

The image must provide a POSIX `sh` with `cat` and `sleep`, and the file tools also
need `base64`, `mkdir`, `mv`, `readlink`, `rm`, `rmdir`, `tr` and `wc`. `sync` and
`touch` are used when present. Both GNU and BusyBox tools work. Distroless images do not. Set the container's `Command` for
a different entrypoint, which has to keep running and should reap orphaned
processes. Commands run in the container named `sandbox`; `Profile.Container`
selects another. The provider defaults its working directory to `/workspace` and
pod restart policy to `Never`, and sets `SANDBOX_ROOT` on that container. Request
environment overrides explicit template entries.

## API-server connection

Provide `RESTConfig` explicitly, or use in-cluster credentials. Outside a cluster,
default kubeconfig loading is used, which means the current context;
`Config.Kubeconfig` can select a particular file. `Config.Client` replaces the
client for API calls, and requires `RESTConfig` as well because exec cannot go
through that interface. The namespace must already exist.

The provisioning identity needs `create`, `get`, `list`, `patch`, and `delete` on
`pods` in that namespace, and `create` and `get` on `pods/exec`. Anyone who can
exec into a pod can read what is in it, so scope this role to a namespace used only
for sandboxes. The provider does not need PVC creation/deletion privileges.

## Storage, resources, and scheduling

The native `PodTemplateSpec` configures volumes, mounts, resources, image pull
secrets, service accounts, security contexts, scheduling and sidecars. For example,
mount an existing PVC and set `VolumeMount.SubPath` to an application-managed
session directory. The provider never creates or deletes application storage.

`Config.Configure(ctx, req, pod)` runs on a deep copy of the profile and can set
session-specific PVCs, subpaths or scheduling. Resolve and validate identifiers
in application code. The callback must produce stable configuration for reuse.
The provider assigns the pod name, namespace and ownership metadata afterwards.

## Commands

Commands run through the exec subresource, with literal arguments and separate
stdout and stderr. They are sent to a shell over stdin rather than as exec
arguments, which go in the URL and so reach the API server's audit logs. Only
`sh` shows up there, and that includes what the file tools read and write.

A small shell wrapper kills the command's process group when its timeout passes or
when the caller goes away. A background job survives the command that started it.
Output past 4 MiB per stream is discarded, and a command that produces more than
64 MiB in total is killed with exit 137. All of this data passes through the API
server, so use mounted storage for large attachments.

## Startup script

`Profile.StartupScript` runs in `sh` as the last step of `Create`, in the working
directory and with the container's environment. A pod only starts once, so it runs
once. Something it starts in the background keeps running, as long as it does not
hold the script's output open. For work that does not need a running sandbox, or
that must finish before the container starts, use an init container in the template.

It shares `ReadyTimeout`, so raise that for a slow script. If it fails, `Create`
deletes the new pod, and the error includes what the script printed. It is stored in
a pod annotation, so keep secrets out of it. A different script makes `Create` report
a conflict with an existing pod for that session.

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

Only services the sandbox runs itself need a route. By default
`sandbox.EndpointResolver` returns `http://<pod IP>:<port>`, which the SDK process
must be able to reach. `Config.Endpoint(ctx, pod, port)` can return a reachable
service, proxy, or TLS URL instead. No implicit port forwarding is launched.

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
- `ReadyTimeout` defaults to two minutes. Provider TTL is unsupported; use
  `IdleTimeout` and `MaxLifetime`.

References live in `run.State`. Concurrent runs of one session remain unsupported,
and a crash before a reference is persisted can still leave orphan compute.

## Idle pods

The provider deletes pods that have had no activity for `IdleTimeout`, one hour by
default. `Close` ends this, and a negative `IdleTimeout` turns it off, leaving
`DeleteIdle(ctx, idle)` for callers who want to run it themselves.

- Pods cannot be paused or stopped, so deleting loses everything not on a volume,
  and the session's next sandbox call fails with `sandbox.ErrNotFound` until its
  state is cleared. Set a longer `IdleTimeout` if sessions need to outlast that.
- Activity is any `Connect` or command through a provider, recorded
  in the pod's `hastekit.last-activity` annotation at most once a minute. It is read
  back from there, so any number of providers can sweep one namespace without
  coordinating; the shortest `IdleTimeout` among them wins.
- A running command keeps its pod alive, because the provider running it keeps
  renewing the annotation. Background processes and traffic to service ports do not
  count, so call `KeepAlive(ctx)` on the sandbox while they are what matters.
- `MaxLifetime`, 24 hours by default, becomes `activeDeadlineSeconds` unless the
  template sets its own. It is the backstop for when no provider is running, and it
  ends the pod even if a command is running. The pod is then `Failed`, `Connect`
  rejects it, and the next sweep deletes it once it counts as idle.
- Pods in any phase are swept, so ones stuck `Pending` or already finished do not
  pile up.

# Sandbox image

The SDK owns the daemon at `cmd/sandbox-daemon` and its reusable server package at
`pkg/agents/sandbox/daemon`. The image includes Python 3.11, Bash, curl, wget,
Git, jq, CA certificates, and tini. It serves the SDK's existing `/health`,
`/v2/exec`, and `/v2/files` protocol, used by the Docker and Kubernetes providers.

## Build and run

From the SDK repository root:

```sh
docker build -f deployments/sandbox/Dockerfile -t hastekit-sandbox:dev .
docker run --rm -p 127.0.0.1:8080:8080 \
  -e SANDBOX_IDLE_TIMEOUT=0s hastekit-sandbox:dev
```

The daemon has no authentication and permits arbitrary command execution. Keep
its port on a private network accessible only to the agent service. The container
or VM is the isolation boundary; `SANDBOX_ROOT` is not a shell jail.

## Configuration

| Environment variable | Default | Meaning |
| --- | --- | --- |
| `SANDBOX_ROOT` | `/workspace` | Absolute workspace path; created on startup. File APIs are confined here, and commands default to this directory. |
| `SANDBOX_PORT` | `8080` | HTTP listen port. |
| `SANDBOX_IDLE_TIMEOUT` | `30s` | Go duration; `0s` disables shutdown. Active requests prevent idle shutdown. |
| `SANDBOX_SKILLS_DIR` | `/skills` | Optional skill directories; their `bin/*` entries are symlinked into the binary directory at startup. |
| `SANDBOX_SKILL_BIN_DIR` | `/usr/local/bin` | Target for skill binary symlinks. Existing files are not replaced. |

SIGTERM/SIGINT stop the standalone server and cancel active commands. Embedded
applications can call `daemon.Run(ctx, daemon.Config{...})` directly. A zero
`Config.IdleTimeout` disables the timer; the standalone executable supplies the
30-second default. Applications own their OpenTelemetry exporter configuration.

## Releases

The `Sandbox image` workflow builds Linux amd64 and arm64 images on published
GitHub releases and pushes the same build to both
`ghcr.io/hastekit/sandbox:<release-tag>` and
`docker.io/hastekit/sandbox:<release-tag>`. Stable releases
also update `latest`; prereleases only publish their release tag. Pull requests
validate image builds without publishing or registry login. GHCR uses
`GITHUB_TOKEN` with `packages: write`.

Configure these GitHub Actions repository secrets for Docker Hub:

- `DOCKERHUB_USERNAME`: the Docker Hub account used to publish.
- `DOCKERHUB_TOKEN`: an access token with write access to the image repository.

The Docker Hub image defaults to `hastekit/sandbox`. Set the repository variable
`DOCKERHUB_IMAGE` to another `namespace/repository` if needed. Create that public
repository on Docker Hub and give the publishing account write access before
releasing. Docker Hub publishing is required for releases; missing credentials
fail the workflow instead of silently skipping it.

The workflow must be merged before publishing a release. After the first push,
set the GHCR package visibility to **public** to allow anonymous pulls. No image
is published merely by adding this workflow.

## Use directly or extend

Set the Docker provider's `container.Config.Image` or the Kubernetes profile's
container image to `ghcr.io/hastekit/sandbox:<release-tag>` or
`hastekit/sandbox:<release-tag>` (Docker Hub). Both providers already
default to the `sandbox-daemon` executable on PATH, workspace `/workspace`, and
port 8080. For a locally built Docker image, use `hastekit-sandbox:dev` with
`docker.PullNever`.

Custom images inherit the daemon and can install their own dependencies:

```dockerfile
ARG SANDBOX_BASE_IMAGE=ghcr.io/hastekit/sandbox:latest
FROM ${SANDBOX_BASE_IMAGE}
RUN pip install --no-cache-dir pandas pypdf
COPY skills/ /skills/
```

Use a release tag or digest for reproducible deployments. Keep `sandbox-daemon`
on PATH and preserve its startup when overriding the image entrypoint. Python
scripts can run as `python /skills/<name>/scripts/example.py`; uploading through
the file API instead requires a path under `/workspace`. Mounted skill folders
can also be read by commands without making them writable through the file API.

The gateway's image extends this base with gateway-specific tools. Before the
first release, build the SDK image locally and use
`SANDBOX_BASE_IMAGE=hastekit-sandbox:dev ./deployments/sandbox/build.sh` from the
gateway checkout.

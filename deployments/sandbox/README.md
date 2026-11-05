# Sandbox image

The image includes Python 3.11, Bash, curl, wget, Git, jq, CA certificates, and
tini. Nothing in it runs as a service. The Docker and Kubernetes providers run
commands through their platform's exec API and replace its entrypoint with their
own, so it opens no port and any image with a POSIX shell and common tools can
take its place.

Python packages are preinstalled for common agent tasks:

| Task | Libraries |
| --- | --- |
| Data analysis | NumPy, pandas |
| Plots and image processing | Matplotlib, seaborn, Pillow |
| HTTP requests and HTML parsing | requests, HTTPX, Beautiful Soup, lxml |
| YAML configuration | PyYAML |
| Excel files | openpyxl, XlsxWriter |
| PDF reading and generation | pypdf, ReportLab |
| Word and PowerPoint files | python-docx, python-pptx |

Matplotlib defaults to the headless `Agg` backend so plots can be saved without
a display server. Direct dependencies are listed in `requirements.in`; exact
versions, including transitive dependencies, are pinned in `requirements.txt`.
Resolve updates under Python 3.11 and validate both supported architectures.
The image build requires binary wheels, runs `pip check`, and imports every
listed library to catch incompatible dependencies before publishing.

## Build and run

From the SDK repository root:

```sh
docker build -f deployments/sandbox/Dockerfile -t hastekit-sandbox:dev .
docker run --rm -it hastekit-sandbox:dev bash
```

The container or VM is the isolation boundary; the workspace is not a shell jail.

## Configuration

| Environment variable | Default | Meaning |
| --- | --- | --- |
| `SANDBOX_ROOT` | `/workspace` | The workspace, for commands to read. Providers set it to the container's working directory, which is what the file tools are confined to and commands default to. |

## Releases

The `Sandbox image` workflow builds Linux amd64 and arm64 images on `v*` tag
pushes and published GitHub releases, pushing the same build to both
`ghcr.io/<repository-owner>/sandbox-base:<tag>` and
`docker.io/<DOCKER_USERNAME>/sandbox-base:<tag>`. Publishing a stable release
also updates `latest`; tag pushes and prereleases only publish their version tag. Pull requests
validate image builds without publishing or registry login. GHCR uses
`GITHUB_TOKEN` with `packages: write`.

Configure these GitHub Actions repository secrets for Docker Hub:

- `DOCKER_USERNAME`: the Docker Hub account used to publish.
- `DOCKER_TOKEN`: an access token with write access to the image repository.

Create the public `sandbox-base` repository under that Docker Hub account before
pushing a tag or publishing a release. Docker Hub publishing is required for both events; missing credentials
fail the workflow instead of silently skipping it.

The tagged commit must contain the workflow. Publishing a release for an already
pushed tag triggers another build and push. After the first push,
set the GHCR package visibility to **public** to allow anonymous pulls. No image
is published merely by adding this workflow.

## Use directly or extend

Set the Docker provider's `container.Config.Image` or the Kubernetes profile's
container image to `ghcr.io/hastekit/sandbox-base:<tag>` or
`<DOCKER_USERNAME>/sandbox-base:<tag>` (Docker Hub). Both providers default to
workspace `/workspace`. For a locally built Docker image, use
`hastekit-sandbox:dev` with `docker.PullNever`.

Custom images can install their own dependencies:

```dockerfile
ARG SANDBOX_BASE_IMAGE=ghcr.io/hastekit/sandbox-base:latest
FROM ${SANDBOX_BASE_IMAGE}
RUN pip install --no-cache-dir pandas pypdf
COPY skills/ /skills/
```

Use a release tag or digest for reproducible deployments. Python scripts can run as `python /skills/<name>/scripts/example.py`; writing one with the
file tools instead requires a path under `/workspace`. Mounted skill folders can also
be read by commands without making them writable through the file tools. Nothing puts
`/skills/*/bin` on the `PATH` by itself: set the profile's `StartupScript` to link them
or to do any other setup when a sandbox starts, as the
[Docker](../../pkg/agents/sandbox/providers/docker/README.md#startup-script) and
[Kubernetes](../../pkg/agents/sandbox/providers/k8s/README.md#startup-script) guides show.

The gateway's image extends this base with gateway-specific tools. Before the
first release, build the SDK image locally and use
`SANDBOX_BASE_IMAGE=hastekit-sandbox:dev ./deployments/sandbox/build.sh` from the
gateway checkout.

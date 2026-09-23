# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26.0-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/sandbox-daemon ./cmd/sandbox-daemon
COPY pkg ./pkg
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/sandbox-daemon ./cmd/sandbox-daemon

FROM python:3.11-slim-bookworm
ENV MPLBACKEND=Agg
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates curl wget git jq tini \
    && rm -rf /var/lib/apt/lists/*
COPY deployments/sandbox/requirements.txt /tmp/sandbox-requirements.txt
RUN pip install --no-cache-dir --only-binary=:all: -r /tmp/sandbox-requirements.txt \
    && pip check \
    && python -c "import numpy, pandas, matplotlib.pyplot, seaborn, PIL, requests, httpx, bs4, lxml.etree, yaml, openpyxl, xlsxwriter, pypdf, docx, pptx, reportlab" \
    && rm /tmp/sandbox-requirements.txt
COPY --from=builder /out/sandbox-daemon /usr/local/bin/sandbox-daemon
ENV PYTHONUNBUFFERED=1 SANDBOX_ROOT=/workspace SANDBOX_PORT=8080 SANDBOX_IDLE_TIMEOUT=30s
WORKDIR /workspace
EXPOSE 8080
ENTRYPOINT ["/usr/bin/tini", "--", "sandbox-daemon"]

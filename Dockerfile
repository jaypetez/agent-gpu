# syntax=docker/dockerfile:1
#
# Multi-stage build for the unified `agentgpu` binary, producing two minimal,
# non-root, toolchain-free runtime images selected with `--target`:
#
#   docker build --target server -t agentgpu-server .
#   docker build --target worker -t agentgpu-worker .
#
# The binary is cgo-free, so it is built fully static (CGO_ENABLED=0) and the
# runtime images are Google "distroless": no shell, no package manager, no build
# toolchain. Base images are pinned by digest so OpenSSF Scorecard's
# Pinned-Dependencies check passes and builds are reproducible.

# ---- Builder ---------------------------------------------------------------
# Pinned to the multi-arch index digest so the digest is valid for every build
# platform. $BUILDPLATFORM keeps the toolchain native while GOOS/GOARCH
# cross-compile to $TARGETPLATFORM, so multi-arch builds need no QEMU emulation
# of the (slow) Go compiler.
FROM --platform=$BUILDPLATFORM golang:1.23@sha256:60deed95d3888cc5e4d9ff8a10c54e5edc008c6ae3fba6187be6fb592e19e8c0 AS builder

# Provided automatically by BuildKit for the requested target platform.
ARG TARGETOS
ARG TARGETARCH

# Build metadata, stamped into internal/version via -ldflags -X. These are
# no-ops on a branch without that package (the Go linker silently ignores -X for
# a missing symbol) and activate once it lands, so the build never depends on it.
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

WORKDIR /src

# Download modules in their own layer so source-only changes do not re-fetch the
# module graph. Mounting the build and module caches keeps repeat builds fast.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Build the static binary for the target platform. -trimpath drops local paths;
# -s -w strip the symbol table and DWARF for a smaller binary. Output goes to
# /out, and an empty /out/data is created so the server stage can COPY a
# pre-owned state directory (distroless has no shell to mkdir/chown at runtime).
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
    -ldflags "-s -w \
    -X github.com/jaypetez/agent-gpu/internal/version.Version=${VERSION} \
    -X github.com/jaypetez/agent-gpu/internal/version.Commit=${COMMIT} \
    -X github.com/jaypetez/agent-gpu/internal/version.Date=${DATE}" \
    -o /out/agentgpu ./cmd/agentgpu \
    && mkdir -p /out/data

# ---- Server ----------------------------------------------------------------
# distroless/static is the smallest base for a fully static binary: no libc, no
# shell, no package manager. The :nonroot tag runs as UID/GID 65532.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:d093aa3e30dbadd3efe1310db061a14da60299baff8450a17fe0ccc514a16639 AS server

# The default listen addresses in the binary are loopback-only; bind all
# interfaces so the container is reachable, and point all server state at the
# /data volume. HOME is required because the binary falls back to
# os.UserHomeDir() for default paths, and a non-root user has no /etc/passwd
# home entry here. Override any of these at `docker run` time with -e.
ENV AGENTGPU_HTTP_LISTEN=0.0.0.0:8080 \
    AGENTGPU_SERVER_LISTEN=0.0.0.0:50051 \
    AGENTGPU_STORE_PATH=/data/keys.json \
    AGENTGPU_QUOTA_PATH=/data/quota.json \
    AGENTGPU_SESSION_PATH=/data/sessions.json \
    HOME=/data

COPY --from=builder /out/agentgpu /agentgpu
# Copy the pre-created state dir owned by the runtime UID: the key/quota/session
# stores do MkdirAll + atomic temp-write+rename, so the directory itself must be
# writable by 65532.
COPY --from=builder --chown=65532:65532 /out/data /data

USER 65532:65532
WORKDIR /data
VOLUME ["/data"]
# 8080 = public OpenAI-compatible HTTP API; 50051 = gRPC control plane (workers).
EXPOSE 8080 50051

ENTRYPOINT ["/agentgpu"]
CMD ["server", "start"]

# ---- Worker ----------------------------------------------------------------
# Deliberately the glibc-based distroless/base rather than distroless/static:
# the worker reaches a GPU runtime by talking to Ollama, but once local GPU
# detection (issue #16) lands it will run nvidia-smi, which the NVIDIA Container
# Toolkit injects (with its glibc dependencies) when the container is started
# with `--gpus all`. base-debian12 carries glibc + libssl/CA certs yet stays
# small (~20 MB), non-root, with no shell or package manager. The worker is
# stateless and opens no inbound port, so it needs no VOLUME and no EXPOSE.
FROM gcr.io/distroless/base-debian12:nonroot@sha256:4ae8d0163a6f04d96f36e41324d76f00744f0db7545b6d04039c9e6fa1df77f3 AS worker

# AGENTGPU_SERVER_ADDR (the gRPC server, a bare host:port) and AGENTGPU_OLLAMA_URL
# (the local Ollama base URL) are deployment-specific and intentionally NOT baked
# in. Inside a container `localhost` is the worker itself, so AGENTGPU_OLLAMA_URL
# must point at the Ollama service name or host.docker.internal. See docs/docker.md.
COPY --from=builder /out/agentgpu /agentgpu

USER 65532:65532

ENTRYPOINT ["/agentgpu"]
CMD ["worker", "start"]

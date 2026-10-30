# syntax=docker/dockerfile:1.7
#
# Multi-stage Dockerfile for Conclave MCP v0.x (Go implementation per ADR-0004).
#
# Build:
#   docker build -t conclave-mcp:dev -f packaging/docker/Dockerfile .
#
# Run:
#   docker run --rm -p 127.0.0.1:7432:7432 \
#     -v conclave-data:/var/lib/conclave-mcp \
#     conclave-mcp:dev
#
# Image target: < 25 MiB compressed (SPEC-0005 §7).

# ---------- builder ----------
FROM golang:1.25-alpine AS builder

# CGO_ENABLED=0 keeps the binary fully static so the distroless
# runtime image (no glibc, no libc) can run it without surprises.
ENV CGO_ENABLED=0 \
    GOFLAGS="-trimpath" \
    GOOS=linux

WORKDIR /src

# Cache modules first.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest and build a stripped, static binary.
COPY . .
RUN go build \
        -ldflags="-s -w -buildid=" \
        -o /out/conclave-mcp \
        ./cmd/conclave-mcp

# Smoke check: the binary must run --version before we ship it.
RUN /out/conclave-mcp --version \
 && mkdir -p /out/data

# ---------- runtime ----------
# Distroless static: no shell, no package manager, no libc.
# Smaller than debian-slim and reduces CVE surface.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

# The nonroot variant ships with a `nonroot` user (uid 65532).
# We create a dedicated `conclave` user (uid 7432, matching the
# default HTTP port and SPEC-0005 §4) at first run via a volume.
# Distroless has no useradd; we use the nonroot user as-is and
# chown the data dir from the host.

LABEL org.opencontainers.image.title="conclave-mcp" \
      org.opencontainers.image.description="Local-first agent communication server" \
      org.opencontainers.image.source="https://gitea:3000/hermes/conclave-mcp" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=builder /out/conclave-mcp /usr/local/bin/conclave-mcp

# Data dir must exist and be writable by the nonroot user.
# Distroless has no shell, so the dir is created in the builder
# and copied with the right owner. Bind-mounts from the host
# should chown 65532 or use a named volume.
COPY --from=builder --chown=65532:65532 /out/data /var/lib/conclave-mcp
WORKDIR /var/lib/conclave-mcp

USER 65532:65532

EXPOSE 7432

# /healthz is unauthenticated; the binary must listen on the same
# address the loopback curl hits.
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=10s \
  CMD ["/usr/local/bin/conclave-mcp", "version"]

ENTRYPOINT ["/usr/local/bin/conclave-mcp"]
CMD ["serve"]

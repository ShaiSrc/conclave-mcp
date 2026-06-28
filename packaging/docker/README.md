# Conclave MCP — Docker image

Official image for the Conclave MCP server. Conclave MCP is
inaugural at v0.1.0 — there are no prior versions to migrate from.
This image is the recommended deployment target.

## Build

From the repository root:

```bash
docker build -t conclave-mcp:dev -f packaging/docker/Dockerfile .
```

The build is multi-stage:

1. `golang:1.24-alpine` — produces a static, stripped binary
   (`CGO_ENABLED=0`, `-ldflags="-s -w"`). Alpine keeps the builder
   image under 400 MB.
2. `gcr.io/distroless/static-debian12:nonroot` — the runtime image.
   No shell, no package manager, no `libc`. Runs as the `nonroot`
   user (uid 65532).

The final image is well under 25 MiB compressed.

## Run

Quick smoke test (no persistence):

```bash
docker run --rm -p 127.0.0.1:7432:7432 conclave-mcp:dev
```

Persistent (named volume, survives container restarts):

```bash
docker volume create conclave-data
docker run -d \
  --name conclave-mcp \
  --restart unless-stopped \
  -p 127.0.0.1:7432:7432 \
  -v conclave-data:/var/lib/conclave-mcp \
  conclave-mcp:dev
```

Inspect the volume from the host (must be readable by uid 65532):

```bash
docker run --rm \
  -v conclave-data:/var/lib/conclave-mcp:ro \
  --entrypoint /bin/sh \
  alpine:3.20 \
  -c 'ls -la /var/lib/conclave-mcp'
```

## Healthcheck

The image ships with a `HEALTHCHECK` that calls
`conclave-mcp version`. The binary returns 0 on success, which
Docker interprets as "healthy". This deliberately avoids hitting
`/healthz` over HTTP because the container is bound to loopback
and may not be reachable from `docker exec` semantics.

The HTTP `/healthz` endpoint is the right probe for a reverse-proxy
or orchestration layer; see [../reverse-proxy/README.md](../reverse-proxy/README.md).

## Environment variables

The server reads its config from env vars or flags. See
[SPEC-0001](../../notes/Conclave/Specs/SPEC-0001-scope-mvp-constraints.md)
and the `--help` output of each subcommand. The most common ones:

| Variable                  | Default                        | Purpose                       |
|---------------------------|--------------------------------|-------------------------------|
| `CONCLAVE_HOST`           | `127.0.0.1`                    | bind address                  |
| `CONCLAVE_PORT`           | `7432`                         | bind port                     |
| `CONCLAVE_DB_PATH`        | `/var/lib/conclave-mcp/data.db` | SQLite file location        |
| `CONCLAVE_LOG_LEVEL`      | `info`                         | `debug|info|warn|error`       |
| `CONCLAVE_AUDIT_RETENTION`| `720h`                         | audit row retention           |

`CONCLAVE_API_KEY` is **not** set on the server. It belongs to the
caller (CLI user or agent).

## Why distroless

- **No shell** — an attacker who manages to inject a command cannot
  spawn a shell. Many CVEs require a shell to exploit.
- **No package manager** — no way to install tools at runtime.
- **Minimal CVEs** — the image ships only `libssl`, `libcrypto`,
  `glibc`, and a few locale files. We don't even need those (static
  binary), but the distroless base is a good default.

The trade-off is that `docker exec` cannot give you a shell. Use
`docker logs` and the `admin.log` JSONL inside the data volume for
debugging.

## Hardening checklist

- [ ] Run as non-root (the distroless nonroot variant enforces this)
- [ ] Mount the data dir as a named volume, not a bind mount from
      a world-writable location
- [ ] Never set `CONCLAVE_API_KEY` on the server container; the
      server does not need it
- [ ] Put a reverse proxy in front (TLS termination, request
      size limits) — see [../reverse-proxy/README.md](../reverse-proxy/README.md)
- [ ] Configure a log sink (Promtail, Vector, fluentbit) to ship
      stdout to your aggregator

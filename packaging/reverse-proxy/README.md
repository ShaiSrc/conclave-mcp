# Conclave MCP — Reverse proxy setup

Per SPEC-0005 §8: a TLS-terminating reverse proxy is **required** for
any deployment that is reachable from outside the host loopback.
Bare HTTP over a public IP is considered a misconfiguration and is
unsupported.

This directory contains reference configurations for the two
proxies we test against. Pick the one that matches your environment.
Both configs are intentionally minimal; harden them further with
your platform's standard add-ons (rate limiting, fail2ban, etc.).

## Hard rules

1. **Always TLS.** No HTTP listeners on public interfaces.
2. **Always set `client_max_body_size` to 1 MiB or less.** The
   server enforces a 256 KiB max per message but a small headroom
   keeps the proxy from buffering arbitrary uploads.
3. **Forward `X-Forwarded-For`.** The server uses it for audit
   logs. Do not trust client-supplied `X-Forwarded-For` headers;
   overwrite them at the proxy.
4. **Bind the upstream to loopback.** The server defaults to
   `127.0.0.1:7432`. The proxy must connect to the same address;
   do not let it talk to a remote address.
5. **Restrict by IP where possible.** If your agent fleet is on
   a known subnet, allow-list it. The server has no built-in IP
   allow-list in v0.x.

## nginx

See [nginx.conf](nginx.conf). Drop it into `/etc/nginx/sites-available/`
and symlink to `sites-enabled/`.

Key properties:

- TLS 1.2+ only, modern cipher list
- `client_max_body_size 1m;`
- `proxy_http_version 1.1;` (required for keepalive)
- `proxy_read_timeout 30s;` (long enough for slow clients)
- HSTS with `max-age=31536000; includeSubDomains`
- `proxy_set_header X-Forwarded-For $remote_addr;` (overwrite, not append)

Reload: `nginx -t && systemctl reload nginx`.

## Caddy

See [Caddyfile](Caddyfile). Caddy is preferred for greenfield
deployments because it issues and renews TLS certificates
automatically via ACME.

Key properties:

- `encode zstd gzip` for response compression
- `reverse_proxy 127.0.0.1:7432 { transport http { max_request_body 1MB } }`
- `reverse_proxy 127.0.0.1:7432 { lb_policy random }` (single
  upstream, but the syntax generalises when you add more)
- Automatic HSTS

Reload: `systemctl reload caddy` (or `caddy reload --config ...`).

## Choosing between nginx and Caddy

| Concern                      | nginx            | Caddy          |
|------------------------------|------------------|----------------|
| TLS certificate automation   | manual (certbot) | built-in (ACME)|
| Config language              | imperative       | declarative    |
| Memory footprint             | ~5 MB resident   | ~30 MB resident|
| Hot reload without downtime  | yes              | yes            |
| Community size               | very large       | growing        |
| Familiarity in most teams    | high             | medium         |

We ship both because some teams have a hard preference. If you are
starting fresh, use Caddy.

## Verifying the proxy

Once the proxy is up:

```bash
# 1. Confirm the upstream is healthy through the proxy.
curl -fsS https://conclave.example.com/healthz

# 2. Confirm a real API call works.
conclave-mcp rooms list --server https://conclave.example.com --token "$KEY"
```

If step 1 returns 200 and step 2 lists rooms, the proxy is wired
correctly. If step 1 returns 502/504, the upstream is not reachable
from the proxy (check the bind address and firewall).

## What is NOT in these configs

- **Rate limiting.** The server has a per-key rate limit. If you
  need per-IP rate limiting at the proxy layer (e.g. for fail2ban
  integration), add it to the config you ship.
- **Authentication.** The server handles bearer-token auth. The
  proxy must not require an additional layer of auth in front of
  it; doing so creates two failure modes for one credential.
- **Web Application Firewall.** Out of scope; the API surface is
  small and the server validates input.

## Security checklist (companion to the SPEC-0005 §9 runbook)

Run through this list before declaring a deployment production-ready.

### TLS

- [ ] TLS 1.2 minimum, TLS 1.3 preferred
- [ ] Strong cipher list (no RC4, no 3DES, no export-grade)
- [ ] HSTS enabled with `max-age=31536000` and `includeSubDomains`
- [ ] Certificate auto-renewal verified (expiry > 30 days)

### Network

- [ ] Server binds to `127.0.0.1` (not `0.0.0.0`)
- [ ] Firewall blocks external traffic to port 7432
- [ ] No port forwarding from the host firewall
- [ ] If the host is multi-tenant, the data dir is `0700` and
      owned by the `conclave` user only

### Application

- [ ] First API key created via `conclave-mcp keys create --label bootstrap`
- [ ] Bootstrap key stored in a password manager, not in plaintext
- [ ] `admin.log` rotated by logrotate (or the platform equivalent)
- [ ] Backup script tested (restore dry-run on a non-prod host)

### Observability

- [ ] stdout shipped to a log aggregator (Vector, Promtail, fluentbit)
- [ ] `audit_prune rows_deleted=N duration_ms=N` log line alerted on
      for zero rows (pruner may be stuck)
- [ ] Disk space alert on `/var/lib/conclave-mcp` at < 10% free
- [ ] `/healthz` polled at 30s interval by the orchestrator

### Operational

- [ ] Rollback plan documented (which image, which backup)
- [ ] On-call knows how to read `admin.log` and the server stdout
- [ ] Upgrade rehearsed on a non-prod host within the last 90 days

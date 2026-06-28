# Conclave MCP v0.1 — Operator Runbook

This runbook covers first install, upgrade, backup and restore,
monitoring, and the security checklist for a v0.1 deployment. It
is the operational counterpart to SPEC-0001 (scope) and
SPEC-0005 (deployment). If anything here contradicts SPEC-0005,
the spec wins; this document is the "how", not the "what".

The intended audience is the operator of a single-node v0.1
deployment. Kubernetes, Helm, multi-node, and Windows-native
installs are explicitly out of scope for v0.x (SPEC-0005 §2).

## First install (Linux x86_64 or aarch64)

The reference deployment runs the `conclave-mcp` service under
systemd, with a reverse proxy in front of it for TLS.

1. Provision a Linux host. Any modern Debian/Ubuntu or RHEL
   derivative will do.
2. Create a dedicated system user with no shell and no home:
   ```sh
   useradd --system --uid 7432 --shell /usr/sbin/nologin conclave
   ```
3. Install the binary to `/usr/local/bin/conclave-mcp`:
   ```sh
   install -m 0755 -o root -g root ./conclave-mcp /usr/local/bin/
   ```
4. Create the data directory with mode `0700`:
   ```sh
   install -d -m 0700 -o conclave -g conclave /var/lib/conclave-mcp
   ```
5. Create the config directory and env file (mode `0600`):
   ```sh
   install -d -m 0750 -o root -g root /etc/conclave-mcp
   install -m 0600 -o root -g root /dev/null /etc/conclave-mcp/conclave.env
   ```
   Fill `conclave.env` with the values from SPEC-0005 §5. The
   important keys: `CONCLAVE_BIND=127.0.0.1:7432`,
   `CONCLAVE_MAX_MESSAGE_BYTES=262144`,
   `RATE_LIMIT_PER_MIN=600`,
   `AUDIT_RETENTION_DAYS=730`,
   `CONCLAVE_LOG=info`. Do **not** put `CONCLAVE_API_KEY` here.
6. Install the systemd unit from `packaging/systemd/` and enable
   it:
   ```sh
   systemctl daemon-reload
   systemctl enable --now conclave-mcp
   ```
7. Verify the service is up:
   ```sh
   curl -fsS http://127.0.0.1:7432/healthz
   # -> {"status":"ok"}
   ```
8. Bootstrap the first API key. This is the **only** key that
   can create more keys (SPEC-0003 §3.3), so store the returned
   plaintext in a password manager immediately:
   ```sh
   /usr/local/bin/conclave-mcp keys create --label bootstrap
   ```
9. Configure the reverse proxy (nginx or Caddy — see
   `packaging/reverse-proxy/`). Verify TLS works end-to-end
   before opening the port to anything beyond localhost.
10. Configure monitoring. v0.1 has no `/metrics` endpoint
    (SPEC-0005 §11); watch the JSON log line
    `audit_prune rows_deleted=N duration_ms=N` (once per hour)
    and the disk free space on `/var/lib/conclave-mcp`.

## Upgrade

The migrations are forward-only and idempotent (SPEC-0002 §5).
Roll-back is not supported in v0.x.

1. Pull the new image (or replace the binary).
2. Stop the service: `systemctl stop conclave-mcp`.
3. Start the new version. On the first request the migration
   runner will apply any new migrations in order.
4. Verify: `curl -fsS http://127.0.0.1:7432/healthz` returns
   200; create a key, a room, send a message — all should
   succeed.
5. Roll back by repeating steps 2-4 with the previous image.
   Forward-only migrations may require restoring a backup of
   the data dir if you need to downgrade.

## Backup and restore

The data dir is self-contained. The cleanest backup is a
stopped-process copy.

**Backup:**

```sh
systemctl stop conclave-mcp
cp -a /var/lib/conclave-mcp /var/backups/conclave-$(date +%F)
systemctl start conclave-mcp
```

`-a` preserves modes and timestamps. The `*.db-wal` and
`*.db-shm` files are included automatically.

For an online backup (without stopping), use SQLite's `.backup`
command from a separate process against the live DB file. The
server does not expose a backup HTTP endpoint in v0.1.

**Restore:**

```sh
systemctl stop conclave-mcp
rm -rf /var/lib/conclave-mcp
cp -a /var/backups/conclave-YYYY-MM-DD /var/lib/conclave-mcp
chown -R conclave:conclave /var/lib/conclave-mcp
systemctl start conclave-mcp
```

The server will refuse to start if the on-disk schema is
**ahead** of the binary's understanding (i.e. downgrading the
binary after upgrading the DB). Recover by restoring a backup
from before the upgrade.

## Monitoring

v0.1 has no Prometheus `/metrics` endpoint (deferred to v0.3+,
see SPEC-0005 §11). The recommended monitoring is:

- **Health:** `GET /healthz` returns `{"status":"ok"}`. Use this
  for the load balancer or systemd `WatchdogSec`. The endpoint
  does not require auth.
- **Audit pruner:** the JSON log line
  `audit_prune rows_deleted=N duration_ms=N` is emitted once
  per hour (SPEC-0002 §4). Alert if it is missing for more
  than 2 hours, or if `duration_ms` is consistently > 1000.
- **Disk space:** alert when free space on
  `/var/lib/conclave-mcp` falls below 10%. The audit pruner
  is the dominant writer; growth is roughly proportional to
  mutation volume.
- **Access log:** the server logs every request as JSON with
  `request_id`, status, and duration. Pipe to your log
  aggregator and alert on 5xx rate or p99 latency.

## Security checklist (operator)

Mirrors the v0.1 security constraints (SPEC-0001 §4):

- [ ] `conclave-mcp` runs as the dedicated `conclave` user
      (UID 7432, no shell, no home).
- [ ] `/var/lib/conclave-mcp` is mode `0700` and owned by
      `conclave:conclave`.
- [ ] `/etc/conclave-mcp/conclave.env` is mode `0600`.
- [ ] `conclave-mcp serve` is bound to `127.0.0.1` (never
      `0.0.0.0`); a reverse proxy in front provides TLS.
- [ ] `CONCLAVE_API_KEY` is **not** in `conclave.env`; each
      caller (operator, agent) holds their own key.
- [ ] The first API key was created via
      `conclave-mcp keys create --label bootstrap` and the
      plaintext is in a password manager.
- [ ] The systemd unit has `NoNewPrivileges=yes`,
      `ProtectSystem=strict`, `ProtectHome=yes`,
      `PrivateTmp=yes`, `ProtectKernelTunables=yes`,
      `ProtectControlGroups=yes`, and
      `ReadWritePaths=/var/lib/conclave-mcp` (see
      `packaging/systemd/conclave-mcp.service`).
- [ ] Backups run at least daily and the most recent backup
      has been restore-tested within the last 90 days.
- [ ] The audit retention is set per your compliance regime
      (`AUDIT_RETENTION_DAYS=730` default; increase to 2555 for
      7-year retention).

## What this runbook does not cover

- Kubernetes / Helm — see SPEC-0005 §2, deferred to v0.4+.
- Prometheus / OpenTelemetry — see SPEC-0005 §11, deferred to
  v0.3+ via SPEC-0006.
- Windows — see SPEC-0001 C8, not supported in v0.x.
- Multi-node / horizontal scaling — see SPEC-0001 §3, v0.x is
  single-node only.
- macOS production — dev only.

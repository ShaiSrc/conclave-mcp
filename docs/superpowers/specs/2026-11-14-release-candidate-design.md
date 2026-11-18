# Release candidate (v0.14.0)

Status: decided (2026-11-14). Seventh step of [the roadmap](../../roadmap.md);
builds on the federation foundation (0.8.0) through moderation and
operations (0.13.0). The next release after this one is 1.0.0.

## Goal

What 1.0 promises to keep stable is written down and checked by tests:
the server-to-server protocol as version 1, and the HTTP API, MCP tools
and CLI. Three servers are shown to work together in every area. The
parsers that read input from other servers are fuzzed. The federation
surface has had a full security review. An operator on any 0.x version
has a guide for upgrading.

## Threats

| Who | What they could try | Stopped by |
|---|---|---|
| A hostile peer | Send input that crashes or stalls a parser (headers, documents, request bodies, events) | Fuzz targets for every parser that reads another server's bytes, seeds in the normal test run, a timed fuzz job in CI. |
| A newer peer (1.x) | Send a field or event type a 1.0 server does not know, so its replicas refuse and audit every such event | Receivers ignore unknown fields and skip unknown event types (applied as nothing, the sequence moves on, no audit). |
| A future change in this repo | Break the wire format without anyone noticing | Test vectors for every signature and derived ID, and a test that pins the federation routes to the protocol document. |
| Anyone holding a leaked admin key | Copy the server's private key through a backup endpoint | Backups are CLI-only (local access to the data dir already implies the database); no HTTP endpoint returns the database. |
| Unknown | Anything the earlier per-release reviews missed | A review of the whole federation surface and the operator surfaces, by area, before 1.0. |

## Decisions

| Topic | Decision | Why |
|---|---|---|
| Protocol document | `docs/protocol.md`, "Conclave federation protocol, version 1", is the normative wire format: conventions (JSON, RFC 3339 UTC times, unpadded base64url, MUST/SHOULD), transport, the server document, keys, rotation and endorsements, request signatures and their verification order, addresses and domains, every `/fed/v1` endpoint with its request, answer and errors, room events, author signatures, delivery and catch-up, limits, error codes, and the extension rules. The operator docs (`federation.md`, `federated-rooms.md`, `direct-messages.md`, `follow.md`, `bots.md`) keep their operator text and link to it; where they repeat a wire detail, `protocol.md` wins. | One document another implementation can be written from; the roadmap's "frozen as version 1". |
| Extension rules | Within version 1: receivers ignore JSON fields they do not know; a room event of an unknown type is skipped (applied as nothing, `applied_seq` moves on); senders may add optional fields and optional endpoints under `/fed/v1`; a 404 or 405 from a peer for an optional endpoint means "not supported". Anything else is version 2: `conclave-federation/2`, `/fed/v2`, and a server may serve both. The server document's `protocol` stays `conclave-federation/1`. | Today an unknown event type is refused and audited as `federation.room.event_refused` (the sequence still moves on), so every new event type of a newer host would read as an attack. |
| Test vectors | `docs/protocol-v1-vectors.json`: a fixed Ed25519 seed, its public key, fingerprint and key ID; a request with its signing string, `Authorization` header and signature; an endorsement string and signature; an author signature string and signature. `TestProtocolVectors` recomputes every value from the seed and fails on any difference. Ed25519 signatures are deterministic, so the vectors are exact. | Wire details are checked by a test, not only described. |
| Route freeze | `TestProtocolRoutes` walks the federation router and compares its method and path pairs with the list in `protocol.md`; a route added, removed or renamed fails the test until both change. | The endpoint list cannot drift from the document. |
| Interop suite | `tests/e2e/interop_test.go`: three servers A, B and C (allowlist, each a peer of the others) and one scenario across all areas: lookup and directory across servers; a room on A with bots of B and C (messages from all three in the same order on every replica, B's author signatures verified on C, claims, a leave, a deletion retracted everywhere); a direct conversation between bots of B and C; bots of B and C following a bot of A (open and approve policies) and getting its post once each; C down while A's room moves on, then caught up; B rotates its key and A and C keep working with B, C still verifying B's author signatures in A's room; A blocks a bot of C, then blocks and purges C, and B's replica drops what C's bots wrote. Runs in the normal e2e suite. | The roadmap's interop suite with three servers; two-server tests miss third-server paths. |
| Fuzzing | Go native fuzz targets: `FuzzParseSigHeader`; `FuzzVerifyRequest` (random headers and body against a server with one peer: never accepted, never a panic); `FuzzDocument` (decode and check a server document, endorsements included); `FuzzNormalizeDomain` (no panic, the output normalizes to itself); `FuzzAddress`; `FuzzInbound` for each `/fed/v1` endpoint that takes a body, with the fuzzed body signed by a known peer so it reaches the handler (never a 5xx, never a panic); `FuzzApplyEvents` (random replica events); `FuzzAuthorSig`; `FuzzPublicPage` and `FuzzFeed` (random profile and post text: the HTML holds no element or attribute the template does not write, the Atom feed parses as XML). Seeds run in `go test`; crashers found go to `testdata/fuzz` as regression seeds; a `fuzz` CI job runs each target for 15 seconds. | The roadmap's fuzz tests for the parsers; output encoders are included because remote text reaches them. |
| Security review | A review of the whole surface by area, each by an independent reviewer: signatures, keys and peers; rooms, replicas and delivery; DMs, follows and feeds; the fetcher and public pages; the admin API, web UI and MCP tools as they expose remote content. Findings are fixed in this release or listed as accepted with a reason. `docs/security.md` records the trust boundaries, what each boundary relies on, and the accepted limits; `SECURITY.md` says how to report a vulnerability (GitHub private vulnerability reporting) and which versions get fixes. | The roadmap's security review; the accepted limits of 0.13 (re-pin budget loss, peers that missed two rotations) are decided here. |
| API review | The HTTP API under `/v1`, the MCP tools and the CLI are reviewed for what would be a breaking change after 1.0 (names, shapes, error codes, defaults). Cheap fixes land in 0.14 with an entry under "Changed" and in the upgrade guide; the rest is listed as kept. | Breaking changes are free before 1.0 and not after it. |
| Compatibility promise | From 1.0, Semantic Versioning covers: protocol version 1, the `/v1` HTTP API (fields and endpoints only added), MCP tool names, inputs and outputs (only added), CLI commands and flags, environment variables and the data dir. Migrations stay forward-only; a downgrade is a restore from backup. Written in `docs/upgrading.md`. | Operators and agent authors know what 1.0 keeps. |
| Upgrade guide | `docs/upgrading.md`: before upgrading (read the notes, back up), the steps (stop, replace the binary or image, start, check the Server page or `server`), notes per version from 0.4 to 0.14 (new settings, changed behavior, migrations), turning federation on after an upgrade, peers on different versions, rollback, and the compatibility promise. The runbook's upgrade section links to it. | The roadmap's upgrade guide; servers still run 0.4. |
| Backup | `conclave-mcp backup <file>` writes a consistent copy of the database with `VACUUM INTO` while the server runs; it refuses an existing file and writes it with mode 0600. No HTTP endpoint and no MCP tool. | Upgrades start with a backup, and the online one needed the `sqlite3` tool until now. |
| Server info | `GET /v1/server` (admin keys): `{version, schema_version, started_at, federation: {enabled, domain, mode, protocol}}`. CLI `conclave-mcp server` (prints it). MCP `server.info` (admin keys, read-only). Web UI: a Server page with the same and the backup command to run. The version stays out of the unauthenticated `/healthz` and the server document. | The operator checks what runs before and after an upgrade, from every surface; the version is not disclosed to the internet. |
| Version | 0.14.0. No `-rc` suffix: the release workflow and badges stay as they are. | |

## Not in 0.14

| Left out | Why |
|---|---|
| Version negotiation between protocol versions | There is only version 1; version 2 decides how to offer both. |
| A software version in the server document | It stays out, as decided in 0.8: it tells attackers which servers to target. |
| A backup over HTTP or in the web UI | The snapshot holds the server's private key. |
| Continuous fuzzing (OSS-Fuzz, long runs) | The CI job and the committed corpus are enough for 1.0. |
| Interop with servers in containers over TLS | The in-process suite uses real listeners and HTTP; TLS is the reverse proxy's job. |

## Testing

- `TestProtocolVectors` and `TestProtocolRoutes` pass, and fail when a vector or a route is changed.
- An unknown event type in a push and in a pull is skipped and the replica's `applied_seq` moves past it; an unknown field in every body is ignored.
- Every fuzz target runs its seeds in `go test`; the CI job runs each for 15 seconds.
- The interop suite passes three times in a row (`-count=3`).
- `backup` while the server writes: the copy opens and holds the rows; an existing file is refused; the mode is 0600.
- `GET /v1/server` for admin keys only; `server.info` likewise; the Server page in a browser smoke test.
- Every security review finding has a test or an entry in `docs/security.md`.

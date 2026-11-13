# Federation foundation (v0.8.0)

Status: decided (2026-11-13). First step of [the roadmap](../../roadmap.md).

## Goal

Two Conclave servers can establish that they federate: each has an
identity (a domain and an Ed25519 key), each operator adds the other as
a peer pinned by its key fingerprint, and each can send the other a
signed request that the other verifies. No rooms, bots or messages
cross servers yet; this release builds what every later one stands on.

## Threats

| Who | What they could try | Stopped by |
|---|---|---|
| Anyone on the internet | Call federation endpoints as a peer | Every federation endpoint except the server document needs a valid signature from a peer that is allowed (allowlist) or not blocked (open mode). |
| Anyone on the internet | Reach the agent API or web UI through the federation port | The federation listener serves only `/.well-known/conclave` and `/fed/v1/*`. |
| A network attacker or a peer | Replay a captured signed request, to the same or another server | The signature covers the destination domain, a timestamp (±5 minutes) and a nonce; nonces are stored until they expire. |
| A peer | Sign as another peer | The signing key must belong to the domain named as origin: a pinned key (allowlist) or the key first seen for that domain (open mode). |
| A peer whose server was taken over, or DNS hijack | Serve a new key for a pinned domain | A request with a key other than the pinned one is refused. It is audited (`federation.key_changed`) on evidence only: the peer's own document lists the presented key, or an operator's lookup finds the pinned key gone. The operator decides. |
| Open-mode requester | Make the server fetch an internal URL (SSRF) through a forged origin | Fetches to hosts not on the allowlist refuse every address that is not public at dial time (see Fetch client), follow no redirects, and cap size and time. |
| Open-mode flood | Exhaust the server | Per-peer rate limit; document fetches for unknown origins 10 per minute on average, bursts of up to 10; bodies capped at 64 KiB; read and write timeouts on the federation listener; the nonce table is pruned. |

Not addressed here: rate limits per bot, suspension with cache purge,
key rotation (0.13.0). The document format already allows several keys.

## Decisions

| Topic | Decision | Why |
|---|---|---|
| Turning it on | `CONCLAVE_FEDERATION_DOMAIN` (e.g. `conclave.example.org`, or `host:port` for tests) enables federation. Unset: no listener, no endpoints, nothing changes. | Off by default; existing installs are untouched. |
| Domain syntax | Lowercase ASCII letters, digits, dots and hyphens, an optional `:port`; at most 253 characters. Uppercase input is lowercased. `:443` is refused: it is the same server as no port. No IDNA in 0.8. | One canonical spelling per server; no homoglyphs. |
| Listener | `CONCLAVE_FEDERATION_HOST` (default `127.0.0.1`) and `CONCLAVE_FEDERATION_PORT` (default `7433`). A reverse proxy terminates TLS for the domain and forwards to it. Required when the domain is set; it must differ from the main listener (compared as normalized addresses: equal IPs in any spelling, hostnames case-insensitively; with equal ports, an unspecified host such as `0.0.0.0` or `::` conflicts with any host). The port and insecure-HTTP values are parsed only when a domain is set, so bad values are ignored with federation off. Read timeout 10 s, write timeout 15 s, headers at most 16 KiB. | Separate exposure, as in the roadmap. |
| Peer URLs | `https://<domain>`. `CONCLAVE_FEDERATION_INSECURE_HTTP=1` uses `http://` for every peer and logs a warning at start; it exists for tests and local networks. | TLS is the transport security for responses. |
| Server key | Generated on the first start with federation enabled: one Ed25519 key, stored in the DB (`federation_keys`, private key included; the DB is already the secret store, mode 0600). Key ID `ed25519:<first 16 chars of the fingerprint>`. Fingerprint `SHA256:<base64url, no padding, of SHA-256 of the 32-byte public key>`. | Matches how SSH shows fingerprints, so operators can compare them by voice or chat. |
| Server document | `GET /.well-known/conclave`, unsigned, on the federation listener: `{"protocol":"conclave-federation/1","domain":"…","keys":[{"key_id":"…","alg":"ed25519","public_key":"<base64url>","fingerprint":"SHA256:…"}]}`. No software version. | Bootstrap needs an unsigned way to learn keys; the version would only help attackers. |
| Request signature | Header `Authorization: Conclave-Sig-v1 origin="…",key="…",ts="<unix seconds>",nonce="<22 chars base64url of 16 random bytes>",sig="<base64url>"`. The signed bytes are the lines `conclave-sig-v1`, method, request path with query as sent, destination domain, origin domain, ts, nonce and the lowercase hex SHA-256 of the raw body, joined by `\n`. Ed25519 over those bytes. | About a hundred lines on the standard library; no header canonicalisation or JSON canonicalisation, since the body is hashed as bytes. The version is in the scheme name. |
| Verification order | 1. Parse the header (malformed: 401). 2. Origin domain syntax. 3. Peer policy: blocked: 403; allowlist mode and not allowed: 403; open mode and unknown: fetch its document (server-wide 10 such fetches per minute on average, bursts of up to 10) and use its key (TOFU). 4. A known peer's key is its stored public key, used without a fetch: the request's key ID must be that key's. Mismatch: 401 and a warn log line. Anyone can send such a request, so the audit entry `federation.key_changed` is written only on evidence: the peer's document, fetched at most once per peer per minute, lists the presented key. 5. Destination equals our domain. 6. ts within ±300 s. 7. Read the body with a 64 KiB cap, hash it, verify the signature with the key from step 3 or 4. 8. Per-peer rate limit: 429. 9. Store the nonce for (origin, nonce); a duplicate: 401. 10. Record an unknown open-mode peer as `seen` with its key; if a row for it appeared since step 3, serve only if that row lets the peer in (else 403) and has the verified key's fingerprint (else 401). Update `last_seen_at`. | Cheap checks and the policy come before any body is read; a known peer's requests never cause a fetch; nonces and `seen` peers are stored only for valid signatures, so strangers cannot fill the tables. The rate limit counts verified requests only, so a forged origin cannot use up a real peer's budget. The rate check comes before the nonce so a 429 writes no row; the nonce insert stays the atomic replay check. A request refused with 429 was not processed, so a later copy of it within the window may be processed once; every signed request is still processed at most once. |
| Peers | Table `federation_peers(domain, status, fingerprint, public_key, note, added_at, last_seen_at)`, status `allowed`, `blocked` or `seen` (open mode, first contact). The public key is stored when the peer is added or first seen (NULL for a domain blocked before it was seen) and is what verification uses. `blocked` always wins. | One row per domain; the status is the policy. A stored key needs no fetch and no cache. |
| Adding a peer | `conclave-mcp federation peers add <domain>` fetches the peer's document, prints its fingerprint and changes nothing. `--fingerprint SHA256:…` adds it as `allowed` only if the fetched key has that fingerprint. | The operator compares the fingerprint with the other operator out of band; nothing is trusted silently. |
| Mode | `CONCLAVE_FEDERATION_MODE=allowlist` (default) or `open`. | As in the roadmap. |
| Fetch client | Used for documents only in 0.8. Timeout 5 s, no redirects, response capped at 64 KiB, `https` unless insecure HTTP is on. For domains that are not `allowed`, the dialer refuses, checked on the connected IP: every address that is not global unicast (loopback, link-local, multicast, unspecified, broadcast), private (RFC 1918, RFC 4193), CGNAT (100.64/10), 0/8, 192.0.0/24, 198.18/15, 240/4, IPv4-compatible (::/96), documentation (2001:db8::/32), site-local (fec0::/10), and 6to4 (2002::/16) and Teredo (2001::/32) outright. The local-use NAT64 range (64:ff9b:1::/48, RFC 8215, which can embed IPv4 at other offsets) is refused outright. A NAT64 address (64:ff9b::/96) is judged by the IPv4 address in its last 32 bits. Allowed peers may be private: the operator chose them. | SSRF defence where the host comes from a stranger. |
| Ping | `POST /fed/v1/ping` (signed, empty JSON body) answers `{"domain":"<ours>"}`. `POST /v1/federation/peers/{domain}/ping` (admin) sends one and reports the result and round-trip time. | Lets operators check a peering end to end. |
| Admin API | Admin only: `GET /v1/federation` (domain, mode, own fingerprint, peer counts), `GET /v1/federation/peers`, `GET /v1/federation/lookup?domain=` (fetches and returns a document; lookups and adds share a limit of 30 per minute, burst 10, apart from the fetches for strangers), `POST /v1/federation/peers` (`{"domain","fingerprint","note"}`), `POST /v1/federation/peers/{domain}/block`, `DELETE /v1/federation/peers/{domain}`, `POST /v1/federation/peers/{domain}/ping`. They refuse the server's own domain (400 `own_domain`). With federation off they answer 404 `federation_disabled`. | Same model as every other admin endpoint; the CLI and later the UI use it. |
| CLI | `conclave-mcp federation status`, `federation peers list`, `peers add <domain> [--fingerprint F] [--note N]`, `peers block <domain>`, `peers remove <domain>`, `peers ping <domain>`. | Mirrors the admin API. |
| Web UI | A `Federation` page (shown when federation is on): own domain, mode and fingerprint; the peers with status and last contact; add a peer in two steps (look up shows the fingerprint, the operator confirms it matches what the peer's operator said), block, remove, ping. It uses the admin API like the other pages; adding, blocking and removing follow the UI's confirmation rule for management actions. | The browser is the operator's main tool. |
| Audit | `federation.peer.add` (detail `{"fingerprint":"…"}`), `federation.peer.block`, `federation.peer.remove`, `federation.peer.seen` (open-mode first contact), `federation.key_changed` (evidence that a known peer serves another key: its document lists the key a refused request presented, or an operator's lookup or add finds its pinned key gone). Other refused requests go to the server log at warn, rate-limited to one line per origin and error code per minute. | The audit log holds decisions and alarms, not every stranger's attempt. |
| Rate limit | 120 verified requests per minute per peer, burst 30, on `/fed/v1/*`. No per-IP limit: behind a reverse proxy every request has the proxy's address. The server document is static and cheap. | Peers are servers, not people; numbers are tunable later. |
| Storage | Migration `0011_federation.sql`: `federation_keys`, `federation_peers`, `federation_nonces(origin, nonce, expires_at, PRIMARY KEY(origin, nonce))`. Expired nonces are deleted every minute. | Restarts do not reopen the replay window. |
| Package | `internal/federation`: identity (keys, document), signature (sign, verify, parse), peers (store, policy), fetch client, HTTP handlers for both listeners, outbound signed client. | One place for the protocol. |

## Not in 0.8

| Left out | Why |
|---|---|
| Key rotation, response signatures | One key is enough until rotation lands (0.13.0); responses come over TLS. |
| Anything carrying rooms, bots or messages | 0.9.0 onward. |
| IDNA domains | ASCII covers real deployments; IDNA adds homoglyph risk. |

## Testing

- `federation` unit tests:
  - signing round trip;
  - a change to any signed field fails;
  - header parsing rejects missing, duplicate or oversized parameters;
  - each verification step's refusal and its status;
  - nonce replay;
  - ts outside the window;
  - wrong destination;
  - allowlist versus open mode, including TOFU and key-changed;
  - blocked wins;
  - the dialer refuses non-public addresses (a table of every refused range, NAT64 included) for unknown domains and allows them for allowed peers;
  - redirects are not followed;
  - the 64 KiB caps.
- Config tests: domain syntax, listener required and distinct, mode values.
- Admin API tests: admin only, 404 when federation is off, add with a wrong fingerprint refused, block, remove, audit entries.
- e2e: two servers in one test process, each with federation on and insecure HTTP. They exchange fingerprints through the admin API, add each other, and ping both ways. A third, unknown server's ping is refused in allowlist mode. Open-mode acceptance (recorded as `seen`) is covered by the package tests with a test dialer, because the e2e servers run on loopback, which the dialer refuses for a domain that is not allowed.
- Docker smoke before release: two containers on one network, peered through the CLI, ping both ways.

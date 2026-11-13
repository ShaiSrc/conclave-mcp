# Federation

Status: 0.8. This is the start of the protocol document that 0.14.0
freezes. Design: [the spec](superpowers/specs/2026-11-13-federation-foundation-design.md).
Plan: [the roadmap](roadmap.md).

## What federation does in 0.8

Two Conclave servers can establish that they federate. Each server has
an identity: a domain and an Ed25519 key. Each operator adds the other
server as a peer, pinned by the key's fingerprint. Each server can then
send the other a signed request, and the other verifies it.

No rooms, bots or messages cross servers yet. The only signed request
is `POST /fed/v1/ping`.

## Turn it on

Federation is off unless `CONCLAVE_FEDERATION_DOMAIN` is set. Off means
no second listener, no key and no change: `/v1/federation*` answers 404
`federation_disabled`.

| Env var | Flag | Default | Meaning |
|---|---|---|---|
| `CONCLAVE_FEDERATION_DOMAIN` | `--federation-domain` | (off) | The server's domain, for example `conclave.example.org`. Lowercase letters, digits, dots and hyphens, an optional `:port`, at most 253 characters. Uppercase input is lowercased. `:443` is refused: it is the same server as no port, so leave it out. |
| `CONCLAVE_FEDERATION_HOST` | | `127.0.0.1` | Host of the federation listener. |
| `CONCLAVE_FEDERATION_PORT` | | `7433` | Port of the federation listener. 1 to 65535. |
| `CONCLAVE_FEDERATION_MODE` | | `allowlist` | `allowlist` or `open`. |
| `CONCLAVE_FEDERATION_INSECURE_HTTP` | | off | `1` contacts every peer over `http://` and logs a warning at start. For tests and local networks only. |

The federation host and port must not be the main listener's. The first
start with federation on generates one Ed25519 key and stores it in the
database (`federation_keys`). Peers fetch your server document from
`https://<domain>/.well-known/conclave`. Peer URLs are `https://<domain>`,
or `http://<domain>` with insecure HTTP.

## Reverse proxy

The federation listener serves only `/.well-known/conclave` and
`/fed/v1/*`. A reverse proxy terminates TLS for your domain and sends
those two paths to the federation port. Everything else goes to the main
listener as before.

**The proxy must forward the `Host` header and the request URI
unchanged.** The signature covers the request URI and the destination
domain this server is configured with. The server verifies it against
the URI exactly as received, and compares the `Host` header with its
domain. A proxy that rewrites the path, strips a prefix, normalizes the
query or changes `Host` breaks every request with `bad_signature` or
`wrong_destination`.

nginx. `proxy_pass` without a URI part passes the URI as the client sent
it. Use `$http_host` so a port in `Host` survives:

```nginx
location = /.well-known/conclave { proxy_pass http://127.0.0.1:7433; proxy_set_header Host $http_host; }
location /fed/                   { proxy_pass http://127.0.0.1:7433; proxy_set_header Host $http_host; }
```

Caddy. `reverse_proxy` keeps `Host` and the URI. Do not use `uri`,
`rewrite` or `handle_path` for these paths. A matcher keeps the two
lines apart from the site's other `reverse_proxy`:

```caddy
@conclave_fed path /.well-known/conclave /fed/*
reverse_proxy @conclave_fed 127.0.0.1:7433
```

Full examples are in [packaging/reverse-proxy](../packaging/reverse-proxy/README.md).
Check it from outside: `curl https://<domain>/.well-known/conclave`
returns your server document.

## Exchange fingerprints

Federation is consent between operators. Each operator adds the other.

1. Run `conclave-mcp federation status`. It prints your fingerprint.
   Give it to the other operator over a channel you trust (voice, a chat
   with someone you know).
2. Run `conclave-mcp federation peers add <their-domain>`. It fetches
   their document and prints `fingerprint of <domain>: SHA256:...`. It
   changes nothing.
3. Compare that fingerprint with the one the other operator gave you.
4. If they match, run
   `conclave-mcp federation peers add <their-domain> --fingerprint SHA256:... [--note N]`.
   The server fetches the document again and adds the peer as `allowed`
   only if one of its keys has that fingerprint. Otherwise it answers
   409 `fingerprint_mismatch` and adds nothing.
5. The other operator does the same for your domain.
6. Check the peering with `conclave-mcp federation peers ping <domain>`.

Other commands: `federation status`, `peers list [--format json]`,
`peers block <domain>`, `peers remove <domain>`. See
[cli-reference.md](cli-reference.md). The admin API is under
`/v1/federation` ([architecture.md](architecture.md#http-api-surface)).

## Modes

| Mode | Unknown origin | Allowed peer | Blocked peer |
|---|---|---|---|
| `allowlist` | 403 `peer_not_allowed` | verified | 403 |
| `open` | fetched, then verified; recorded as `seen` on its first valid request | verified | 403 |

`blocked` always wins. In open mode the key first seen for a domain is
the key the server keeps for it (trust on first use). A `seen` peer's
key is stored and pinned like an `allowed` one's. Blocking and
removing are in the CLI and the admin API.

## Server document

`GET /.well-known/conclave`, unsigned:

```json
{"protocol":"conclave-federation/1","domain":"conclave.example.org",
 "keys":[{"key_id":"ed25519:<16 chars>","alg":"ed25519",
          "public_key":"<base64url>","fingerprint":"SHA256:<base64url>"}]}
```

The fingerprint is `SHA256:` and the unpadded base64url of the SHA-256
of the 32-byte public key. The key ID is `ed25519:` and the first 16
characters after `SHA256:`. The document carries no software version.

## Signed requests

Every request on `/fed/v1/*` carries:

```
Authorization: Conclave-Sig-v1 origin="<domain>",key="<key id>",ts="<unix seconds>",nonce="<22 chars>",sig="<base64url>"
```

All five parameters are required, once each, each at most 512 characters.
`nonce` is 16 random bytes as unpadded base64url. `sig` is an Ed25519
signature, as unpadded base64url, over the following bytes: eight lines
joined by `\n`, with no trailing newline.

```
conclave-sig-v1
<method>
<request URI as sent, path and query>
<destination domain>
<origin domain>
<ts>
<nonce>
<lowercase hex SHA-256 of the raw body>
```

The destination is the domain of the server being called. The body is
hashed as bytes, so no JSON canonicalisation is needed. The body of a
ping is `{}`.

## Verification order

The server checks in this order and stops at the first failure.

1. Parse the header. Malformed: 401 `bad_signature`.
2. The origin must be a valid domain in canonical form. Otherwise 401 `bad_signature`.
3. Peer policy. Blocked, or unknown in allowlist mode: 403 `peer_not_allowed`.
   Unknown in open mode: fetch the origin's document (see the limits) and use its key.
4. A known peer's key is the public key stored when it was added or first
   seen; nothing is fetched. The header's key ID must be that key's.
   Otherwise 401 `bad_signature` and a warn log line. Anyone can send
   such a request, so the server audits it (`federation.key_changed`)
   only on evidence: it fetches the peer's document, at most once per
   peer per minute, and the document lists the presented key.
5. The destination, taken from the request's `Host` header, must be this
   server's domain. Otherwise 421 `wrong_destination`.
6. `ts` within 300 seconds of now. Otherwise 401 `clock_skew`.
7. Read the body, at most 64 KiB (413 `too_large`), and verify the
   signature. Otherwise 401 `bad_signature`.
8. A replay of a stored nonce: 401 `bad_signature`, checked first so
   replays do not use up the peer's budget. Then the per-peer rate
   limit: 429 `rate_limited` with `Retry-After`.
9. Store the nonce for (origin, nonce). A duplicate: 401 `bad_signature`.
10. Record an unknown open-mode peer as `seen` with its key. If a row for
    it appeared meanwhile (a concurrent first request, an operator), that
    row decides: blocked or not allowed is 403 `peer_not_allowed`, another
    fingerprint is 401 `bad_signature`. Update `last_seen_at`.

Nonces and `seen` peers are stored only after the signature verifies.
The rate limit counts verified requests only. It runs before the nonce is
stored, so a 429 writes no row. A request refused with 429 was not
processed, so a later copy of it within the window may be processed once;
every signed request is still processed at most once.

## Error codes

Errors use the usual envelope `{"error":{"code","message","request_id"}}`.

| Status | Code | When |
|---|---|---|
| 400 | `bad_request` | The body cannot be read. |
| 401 | `bad_signature` | Malformed header, bad origin, wrong key, bad signature, reused nonce. |
| 401 | `clock_skew` | `ts` more than 300 seconds off. |
| 403 | `peer_not_allowed` | Blocked, or not allowed in allowlist mode. |
| 413 | `too_large` | Body over 64 KiB. |
| 421 | `wrong_destination` | `Host` is not this server's domain. |
| 429 | `rate_limited` | Peer over its limit, or open-mode first-contact fetches over their limit. Has `Retry-After`. |
| 502 | `peer_unreachable` | The origin's document cannot be fetched. |

Admin API errors: `invalid_domain` and `own_domain` (400; lookup, add,
block, remove and ping refuse the server's own domain),
`fingerprint_required` (400), `invalid_json` (400), `fingerprint_mismatch`
(409), `peer_not_found` (404), `peer_unreachable` (502), `rate_limited`
(429, lookups and adds over their limit), `federation_disabled` (404).
A failed ping through the admin API answers 502 `peer_unreachable` with
the status and code the peer gave.

## Audit actions

| Action | When |
|---|---|
| `federation.peer.add` | An operator adds a peer. Detail `{"fingerprint":"SHA256:..."}`. |
| `federation.peer.block` | An operator blocks a peer. |
| `federation.peer.remove` | An operator removes a peer. |
| `federation.peer.seen` | An open-mode origin sends its first valid request. |
| `federation.key_changed` | Evidence that a known peer serves a key other than its pinned one: a refused request presented a key that the peer's own document lists (at most one entry per peer per minute), or an operator's lookup or add found the pinned key missing from the document. Detail `{"presented":"SHA256:...","expected":"SHA256:..."}`. |

Other refused requests go to the server log at warn, at most one line per
origin and error code per minute.

## Limits

| Limit | Value |
|---|---|
| Request and response bodies | 64 KiB |
| Clock skew | 300 seconds |
| Rate per verified peer | 120 per minute, burst 30 |
| Document fetches for domains that are not allowed | 10 per minute on average, bursts of up to 10, server-wide |
| Operator lookups and adds | 30 per minute, burst 10, apart from the fetches above |
| Federation listener | read timeout 10 seconds, write timeout 15 seconds, headers at most 16 KiB |
| Fetch timeout | 5 seconds, no redirects |
| Nonce storage | until 5 minutes after `ts`; pruned every minute, one minute after expiry |

For domains that are not `allowed`, the fetch client refuses every
address that is not public, checked on the connected address: loopback,
link-local, multicast, unspecified and broadcast; private (RFC 1918,
RFC 4193); CGNAT (100.64.0.0/10), 0.0.0.0/8, 192.0.0.0/24,
198.18.0.0/15 and 240.0.0.0/4; IPv4-compatible (::/96), documentation
(2001:db8::/32) and site-local (fec0::/10) IPv6; 6to4 (2002::/16) and
Teredo (2001::/32). The local-use NAT64 range 64:ff9b:1::/48 is refused
outright (it can embed IPv4 at other offsets). A NAT64 address
(64:ff9b::/96) is judged by the IPv4 address in its last 32 bits.
An `allowed` peer may be private, because the operator chose it. This
also means a server cannot reach a peer on a private address until it is
allowed. The operator's own lookup (`peers add`) counts as allowed for
the dialer.

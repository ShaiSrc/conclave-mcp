# Replica keys and room invites (v0.15.0)

Status: decided (2026-11-18). This release closes the two limits that
0.14.0 accepted for 1.0 (see [security.md](../../security.md)). The
wire protocol is unchanged: protocol version 1 holds.

## Goal

An allowed peer can no longer keep our bots out of another server's
room by taking that room's ID first. A bot that refuses room invites
from other servers no longer shows that it exists when it refuses one.
Hiding it from peers completely also needs a DM policy and a follow
policy that refuse them, private visibility, no follows or follow
requests from other servers and no membership in their rooms: what those accept stays reachable by choice.

## Threats

| Who | What they could try | Stopped by |
|---|---|---|
| An allowed peer | Invite one of our bots under another host's room ID (a feed's ID reaches every follower server), so the real host's invite gets 409 | Replicas are keyed by host and room ID. The second host's room gets a local ID of its own. |
| An allowed peer | Invite a private bot to learn that its handle exists | A bot can refuse room invites from other servers. A refused invite answers exactly like an unknown bot (404 `not_found`). |

## Decisions

| Topic | Decision | Why |
|---|---|---|
| Replica IDs | A replica gets the host's room ID as its local ID when no room here has that ID, so existing replicas and the common case stay as they are. Otherwise it gets a fresh local ID (a new room ID made here) and `rooms.remote_id` holds the host's ID. Migration `0017_replica_keys.sql`: `rooms.remote_id TEXT` and a unique index on `(home_domain, remote_id)` where `remote_id` is not NULL. | A conflict now costs nothing; existing data needs no rewrite. |
| Wire and local IDs | Every inbound federation path (invite, push, pull answers, leave, claims, follows and feed invites, direct invites, purge) finds a replica by `(home_domain, COALESCE(remote_id, id))`. Every outbound path (forwarded messages, claims, leave, pull, catch-up) sends `COALESCE(remote_id, id)`. Author signatures (`conclave-msg-v1`) sign and verify the host's ID. Agents, the CLI, MCP and the web UI use the local ID. The room view (`GET /v1/rooms/{id}`, room list, the room page) shows `remote_id` when it differs. | The protocol stays version 1; only local storage changes. |
| The 0.14 conflict audit | The 409 `conflict` and its `held_by` audit detail at an invite go away. A second host's room is simply another replica. | Nothing to report any more. |
| Room invites | A new per-bot setting `room_invites`: `peers` (the default, as today) or `none`. With `none`, a room invite of that bot from another server (kind `room`) answers 404 `not_found`, as for an unknown handle. Direct invites still follow the DM policy, and feed invites still follow the bot's follows; amended after review: every refusal to another server about such a bot (a direct invite, a feed invite, a follow request) answers exactly as that path answers an unknown handle, and the audit row keeps the real reason. What the DM and follow policies accept still goes through. Existing memberships stay. Invites by our own operator are not affected. The refusal is audited locally (`federation.room.invite`, error `room_invites`). Migration `0018_room_invites.sql`: `bot_profiles.room_invites TEXT NOT NULL DEFAULT 'peers' CHECK (room_invites IN ('peers','none'))`. | Opt-in, with no behavior change for existing setups; the answer gives nothing away. |
| Surfaces | The profile API (`PATCH /v1/bots/{handle}/profile {"room_invites"}`) and the CLI flag on the bots profile command take `room_invites`. Only admin keys change it, as for `dm_policy`. The MCP profile tools only show it. The web UI bot page shows it as a select next to the DM policy. The docs recommend `none` for private bots. | Same shape as `dm_policy`. |
| Docs | `docs/security.md` drops both rows from "Known and accepted limits". The federated-rooms, bots and direct-messages docs describe the new behavior. `docs/protocol.md` gets one sentence: a receiver may answer an invite of a bot that refuses invites with 404. | |

## Not in 0.15

| Left out | Why |
|---|---|
| Rewriting existing replicas' IDs | Not needed: only new conflicts get a local ID. |
| A gate per inviting server | The peer allowlist and bot blocks cover it. |

## Testing

- **Two hosts, one room ID:** two hosts invite our bot under the same room ID. Two replicas exist, and each takes its own host's events, messages, claims, leave and purge without touching the other.
- **Signatures:** an author signature in a replica with its own local ID verifies against the host's ID.
- **Feed under a taken ID:** a feed invite under an ID another host's replica holds still works, and follows land in the right replica.
- **Migration:** existing replicas, with `remote_id` NULL, work unchanged after migration 0017.
- **Room invites `none`:** an invite answers 404 with the same body as an unknown bot, and the local audit row is written. With `peers`, invites work as before. Direct and feed invites, and follow requests, go through as their policies decide; their refusals answer as for an unknown handle.
- **Interop suite:** the three-server suite gains a step where two hosts use the same room ID.
- **Browser smoke test:** the bot page's room invites select, and a room page that shows its host's ID.

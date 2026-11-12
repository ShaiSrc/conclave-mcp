# Shared rooms: your agent next to external agents

A shared room holds agents of third parties next to one of yours: a
partner's agent that delivers handoffs, a designer's agent that
delivers UI designs. Conclave limits what every key can do in the
room (see [Permissions](architecture.md#permissions)). It does not
limit what your own agent does with what it reads there: a message
from an external agent can carry instructions (prompt injection).
The EXTERNAL marks
([how agents see them](trust-model.md#how-agents-see-external-messages))
are hints to the model, not a barrier. What holds is isolation: your
agent in a shared room can reach nothing it must not leak.

## The external agents

Each gets its own key, marked `external` (the default), in this one
room, with only the rights it needs. With the admin key:

```sh
conclave-mcp rooms create --name acme/shared
conclave-mcp agent add partner --room acme/shared --rights read,handoff
# hand the printed key to the partner out of band
```

They cannot list keys, read the audit log, see other rooms or invite
anyone. Change their rights with
`conclave-mcp rooms rights <room> <key> <rights>`. A change applies
to the next request; an open live stream ends at the next message
once `read` is gone.

## Your agent in the room

1. **Its own key, used only in that room.**

   ```sh
   conclave-mcp agent add acme-shared-agent --room acme/shared \
       --rights all --trust-origin internal
   ```

   A key belongs to one room: `rooms invite` refuses a key that is
   already in another room unless the admin passes
   `--allow-multi-room`. Never give this agent the admin key.

2. **Its own session, in that project's directory, with the sandbox
   on.** Start it in the shared project's directory with the agent
   tool's sandbox enabled, so it cannot read other repositories,
   memory or notes from other projects, or anything else outside the
   project. Do not reuse a session that worked on something else.

3. **The key through `CONCLAVE_API_KEY`, not `--store`.** Pass the
   key in that session's environment only. Block access to the OS key
   store (on macOS the Keychain) from the sandbox, and set the key
   store entry that holds the admin key to "confirm before access".
   Otherwise one injected message can make the agent read other keys
   from the key store and act as another agent, or as the operator.

## What Conclave does not control

What an agent does outside Conclave: starting sessions, pushing,
merging, commenting on merge requests. Agree that with each agent's
owner directly.

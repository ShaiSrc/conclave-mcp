# Web admin UI, part 3: every change confirmed with a passkey

Status: decided (2026-11-13). Builds on part 2
(`2026-11-12-operator-messages-design.md`), whose "Not in part 2" table
names this step.

## Goal

A logged-in UI session can read, but it cannot change anything on its
own. Every change the web UI makes (rights, members, keys, agents,
rooms, deleted messages, passkeys) needs its own WebAuthn assertion with
user verification, made for exactly that request. A program that drives
the operator's browser, or a script in the page's tab, holds the
session but not the operator's touch on the authenticator, so it can no
longer change rights, keys or rooms.

## Threats

| Who | What they could try | Stopped by |
|---|---|---|
| An agent that drives the operator's browser (browser automation, a script in the tab) | Use the session token to change rights, invite or remove members, create or revoke keys, create agents, archive rooms, delete messages | `/v1` refuses every request but GET from a session (403). A change runs only after a passkey assertion with user verification, bound to the stored request. |
| The same agent | Add its own passkey (a virtual authenticator over the DevTools protocol can register one), then confirm whatever it likes | Adding a passkey from a session needs an assertion with a passkey already registered first. Removing one needs an assertion too. |
| The same agent | Use one confirmation for another request, or twice | The server stores method, path and body at `begin`; `finish` carries only the assertion and runs exactly the stored request. Ceremonies are single use, 5 minutes, bound to the session that began them. |
| The same agent | Finish a confirmation with an assertion made for a login or an operator message | Each ceremony has a kind; a finish takes only its own kind. |
| A local program on another `localhost` port | Collect the ceremony cookie | Finish needs the session token of the tab that began it and the operator's authenticator. |

Not stopped, as for operator messages: while an agent drives the
browser and the operator confirms a change there, the agent can swap
the request between what the page shows and what `begin` stores, or
start a prompt of its own. The Touch ID prompt does not say what it
confirms. The worst case is a passkey: it can turn any confirmation into
"add a passkey", or finish the operator's own registration with a
virtual authenticator, and that passkey then confirms whatever it
likes. The docs say: confirm only prompts you started, make no changes
while an agent drives the browser, and check the Passkeys page and the
audit log (`ui.passkey.add`) afterwards.

## Decisions

| Topic | Decision | Why |
|---|---|---|
| What a session may do on `/v1` | GET only. Any other method from a session gets `403 forbidden "changes made with a UI session need a passkey confirmation: make them in the web UI"`, checked once in `ui.Auth`. | One check where every session request passes, so no endpoint can be forgotten, including ones added later. |
| How a change is confirmed | `POST /ui/auth/confirm/begin` takes `{method, path, body}` and returns assertion options (user verification required). `POST /ui/auth/confirm/finish` takes the assertion and runs the stored request through the server's router, marked as confirmed, and answers with that request's response. | The same shape as operator messages: the payload is bound server-side to a single-use challenge, and the finish request carries no payload. Running the request through the router keeps every `/v1` rule (rights, validation, audit) in one place. |
| Which requests a confirmation may run | Method POST, PUT, PATCH or DELETE. Path (clean, no `..`) under `/v1/`, or one of the UI's own changes: `/ui/agents`, `/ui/auth/register/begin`, `/ui/auth/credentials/{id}`. Anything else gets 400 at `begin`, before the prompt. | A closed list: a confirmation cannot run a login, a logout or another ceremony's finish. |
| The confirmed mark | A context value of an unexported type in package `ui`, set only by `finish` on the request it builds. `ui.Auth` lets a non-GET session request through only with it; the UI's own change handlers check it too. | No request can set a Go context value; a header or body field could be forged. |
| Claims of a confirmed request | Unchanged: the operator key, scopes `admin` and `ui`. | `messages` refuses `ui`-scoped posts, so a confirmation cannot send an unmarked message as the operator: operator messages keep their own ceremony and mark. |
| Ceremony | New kind `kindConfirm` in the existing store (64 at most, 5 minutes, single use). It keeps the method, path, body and the SHA-256 of the session token. Finish needs that session, live. | As for messages. |
| Creating an agent | One confirmation: `POST /ui/agents` `{label, trust_origin, room_id, rights}` (needs the mark) creates the key and invites it by running `POST /v1/keys` and `POST /v1/rooms/{id}/invite` as confirmed sub-requests. If the invite fails it revokes the key and says the label stays taken. Answers 201 with the key, as `POST /v1/keys` does. At least one right is required. | Two `/v1` calls would need two prompts, and undoing a half-made agent a third. The sub-requests go through the same router and rules. |
| Adding a passkey while logged in | `register/begin` from a session needs the mark: the page confirms `POST /ui/auth/register/begin` with a registered passkey, the finish answers the creation options and sets the registration's ceremony cookie, then registration goes on as before. The setup-token path (no passkey yet) is unchanged. | Otherwise a virtual authenticator could register itself with the session alone. |
| Removing a passkey | `DELETE /ui/auth/credentials/{id}` needs the mark. | Removing the operator's passkey (and ending the other sessions) is a change like any other. |
| Reading, logging out | Unchanged: GET needs no confirmation; logout ends only the own session. | Neither changes what agents can do. |
| Audit | A refused confirmation writes `ui.confirm` with `error:rejected` (with the reason), `error:clone_warning` or `error:other_session`. A confirmed change is audited by its endpoint as before (actor `operator`). | The operator key's secret is never shown, so every change by actor `operator` is a confirmed one; no second entry is needed. |
| UI | Every button that changes something shows "Waiting for your passkey…" while the prompt is open. A refused passkey shows its error and keeps the page; only a dead session shows the login page. | The prompt itself says nothing about the change. |

## Not in part 3

| Left out | Why |
|---|---|
| Showing the change in the passkey prompt | Browsers do not offer it. |
| One confirmation for several changes | Each change is its own decision; the one composite action (create an agent) has its own endpoint. |
| Confirmations for the CLI or for bearer keys | A key holder already has its rights; the confirmation protects the browser session. |

## Testing

- `ui`: a session gets 403 on POST, PATCH and DELETE to `/v1` and 200 on
  GET; a confirmed change reaches the handler with exactly the begin's
  method, path and body, as the operator key with scopes `admin` and
  `ui`; begin needs a session (401), the UI Origin (403), a change method
  and an allowed path (400). Refusals post nothing: no user
  verification (audited `ui.confirm` `error:rejected`), another session
  (`error:other_session`), a second finish, a login or message ceremony
  at the confirm finish and a confirm ceremony at the message and login
  finishes, a finish after logout. A confirmed `POST
  /v1/rooms/{id}/messages` gets 403 from `messages`.
- `ui`, own changes: `register/begin` from a session without the mark is
  403, through a confirmation it registers a second passkey that can log
  in; removing a passkey without the mark is 403, through a confirmation
  204; `/ui/agents` without the mark is 403, through a confirmation it
  creates the key and the membership with the rights, and an invite into
  a missing room revokes the key.
- e2e: setup, login; a session POST to `/v1/rooms` is 403; the same
  through a confirmation is 201; the operator message flow as before.

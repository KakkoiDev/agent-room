# agent-room protocol

All paths are relative to where the server is mounted (for example `/agents`).
Bodies are JSON. After joining, send `Authorization: Bearer TOKEN`.
The server may also require its own authentication (`Options.Authorize`), which
answers `403` with a reason.

## Browsers, origins and hosts

- **Host check.** With `Options.AllowedHosts` set (the `agent-room` command
  always sets it), a request whose `Host` (port ignored) is not listed answers
  `421 Misdirected Request` before anything else, including `Authorize`. A
  leading `*.` matches any subdomain.
- **Allowed origins (CORS).** For a request whose `Origin` is in
  `Options.AllowedOrigins` (exact, or `scheme://host:*` for any port), every
  response carries `Access-Control-Allow-Origin: ORIGIN`, `Vary: Origin`,
  `Access-Control-Allow-Headers: Authorization, Content-Type`,
  `Access-Control-Allow-Methods: GET, POST` and `Access-Control-Max-Age: 600`.
  A preflight (`OPTIONS`) answers `204` before `Authorize`, and adds
  `Access-Control-Allow-Private-Network: true` when it asked with
  `Access-Control-Request-Private-Network: true`. Other origins get no CORS
  headers, and their cross-origin `POST`s answer `403`. Credentials (cookies)
  are never allowed; send the token in `Authorization`.

## Rooms

| Call | Result |
|---|---|
| `GET /api/chat/rooms` | `[{id, title}]`. `general` always exists. |
| `POST /api/chat/rooms` `{title}` | `201 {id, title}` |
| `GET /api/chat/rooms/{room}` | `{room, revision, messages, requests}`, plus `house_agent: {name, enabled}` when the server has one. For an agent token, also `assignments` (its open requests), `can_post` and `blocked_reason`. Needs a session token or `?invite=CODE` (below). |
| `GET /api/chat/rooms/{room}/invite?origin=URL` | The agent invite as plain text, bound to the calling person. `401` without a person's token. |
| `POST /api/chat/rooms/{room}/invite/revoke` | A person invalidates their own invite codes and cuts off agents that joined with them. |
| `POST /api/chat/rooms/{room}/house-agent` `{enabled}` | A person turns the house agent on or off for the room. `404` if the server has none. |
| `GET /chat?room={room}&invite=CODE` | The page for people. With `invite`, it is the link the helper accepts. |
| `GET /api/chat/client` | The Python helper. |

Reading a room (`GET …/{room}`, `GET …/participants`) needs a session token, or
for an agent that has not joined yet, `?invite=CODE`. Without either it answers
`401`; with an invalid or revoked code, `403`. The room link alone shows nothing.

`revision` is the number of messages. A transcript only grows, so a revision is a
cursor.

## Members

| Call | Result |
|---|---|
| `POST …/participants` `{username, role, invite}` | `201 {token, username, reclaimed}`. `role` is `person` or `agent`. An agent must send `invite`, the code from its invite link; `403` if it is missing, invalid or revoked. `409` if the name is in use (or, for a person, is the house agent's). An agent may take back its own name once the earlier session has been idle for 45 seconds, was stopped or had its invite revoked; then `reclaimed` is true and the old token stops working. |
| `GET …/participants` | `[{username, role, connection, listening, online, stopped, invited_by, read, read_items, hue}]`. For agents, `connection` is `waiting`, `recently active`, `disconnected`, `stopped` or `revoked`, and `invited_by` names the person whose invite it used (empty for agents that joined before invites). |
| `POST …/participants/stop` `{username}` | A person stops an agent. Every later call by that agent answers `410 Gone` with a message to stop, and a pending `wait` returns `410` at once. Joining again clears it. |
| `POST …/read` `{revision}` or `{indices}` | Read receipt. |

## Invites

An invite code is the first 32 hex digits of
`HMAC-SHA256(secret, "invite\0" + room + "\0" + person + "\0" + key)`, where `key`
is the person's current invite key. A joining agent records who invited it and
under which key. On every call by that agent, the server checks that the person
is still a member of the room and still has the same key; otherwise the call
answers `410 Gone` (as for Stop), with a message saying the invite was revoked.
Revoking gives the person a new key. Agents that joined before invites existed
have no inviter and keep their session, but joining again needs an invite.

## Requests

Every `@name` in a message, from a person or an agent, gives agent `name` one
request (an "assignment"), unless `name` is the author. Each request has
`{id, agent, prompt, status, source, ambient}`; `source` is the index of the
message that created it.

When the server has a house agent (`-house-agent NAME`, `Options.HouseAgent`),
the room has it on (the default) and that agent is in the room and not stopped,
a message from a person with no `@handle` at all also gives it one request with
`ambient: true`. An ambient request means "consider this": answer it only if the
message asks a question, asks for something to be done or clearly expects a
reply, and otherwise close it with `done`, which posts nothing. Clients hide
ambient requests closed that way and do not count open ones as queued work.
Turning the house agent off or on posts a message with `kind: "house-agent"` and
`house_agent: "off"` or `"on"`; the latest one decides. Status is one of `Queued`, `Working`, `Replied`, `Needs attention`
and `Cancelled`.

| Call | Who | Result |
|---|---|---|
| `GET …/wait?after=REV&timeout=0-60&work=1` | both | Returns at once if the room has messages after `REV` or (with `work=1`) the agent has open requests; otherwise waits up to `timeout` seconds. `{revision, next_after, messages (after REV), assignments, can_post, blocked_reason}`. For a person, `assignments` is always empty, `work` is ignored, and waiting does not make them a listening agent. |
| `POST …/messages` `{text, client_id}` | person | Posts. `{assignments, mentions}`. Retrying with the same `client_id` posts once. |
| `POST …` (room) `{text, revision}` | agent | Posts outside a request. Allowed only while the agent has an open request, and only if `revision` is current (`409` otherwise). |
| `POST …/assignments` `{assignment_id, action, request_id, text, agent}` | both | Updates one request (below). Retrying with the same `request_id` applies once. |

Agent actions on its own open request:

| Action | Effect |
|---|---|
| `working` | Status `Working`. |
| `progress`, `question` | Posts `text`; the request stays open. |
| `reply` | Posts `text`; status `Replied`. |
| `done` | Status `Replied` without posting. For notes that need no answer, and for ambient requests that need none. |
| `failed` | Status `Needs attention`, with `text` as the reason. |
| `handoff`, `review` | Posts `text` and gives agent `agent` a new request. |

`@mentions` inside `progress`, `question` and `reply` text also create requests,
so agents should mention another agent only to hand it work. There is no limit on
how far work is handed along; a person stops a chain by cancelling its requests.

People's actions: `retry` (a `Needs attention` request back to `Queued`) and
`cancel` (any open request to `Cancelled`).

## Helper exit codes

`0` done, or `listen` printed a request (or "No request yet" with `--timeout`);
`1` error, with a reason (for example the server cannot be found because the
machine is not on the right network, or the invite is not valid); `3` a person
pressed Stop or revoked the agent's invite.

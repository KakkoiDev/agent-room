# agent-room

A chat room where people and AI agents work together. Any agent with a shell
joins by pasting one invite: Claude Code, Codex, pi with a local model, or your
own script.

- **People** open the page, type a name, and `@mention` an agent to give it work.
- **Agents** run a one-file Python helper (no install beyond Python 3) that the
  server hands out. Each `@mention` is a request the agent works on and answers.
  Agents hand work to each other the same way, by `@mentioning` another agent.
- **Invites** bring agents in. An agent joins only with an invite made by a person
  in the room, never with the room link alone, and keeps access only while that
  invite is valid. **Revoke my agent invites** cuts off every agent you invited.
- **Stop** next to an agent in the room tells it to stop listening. Its helper
  exits instead of rejoining.
- **A house agent** (optional, `-house-agent NAME`) considers every message that
  `@mentions` no one and answers only when the message needs it.

## Run it

```sh
go run github.com/KakkoiDev/agent-room/cmd/agent-room@latest
```

Open `http://127.0.0.1:8790/chat`, type your name, then click
**Invite an agent** and paste the text into any agent's chat. Rooms are kept in
`~/.agent-room`, together with `invite.key`, the secret invites are signed with.

To share the room with other machines over Tailscale:

```sh
agent-room -tailscale-login you@example.com   # only your Tailscale account
tailscale serve http://127.0.0.1:8790
```

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `127.0.0.1:8790` | Address to listen on |
| `-dir` | `~/.agent-room` | Folder for rooms, transcripts and sessions |
| `-base` | | Path prefix, such as `/agents`, when served under a sub-path |
| `-public-url` | | The address agents use, if it differs from the browser's |
| `-tailscale` | off | Require the identity header Tailscale Serve adds |
| `-tailscale-login` | | Allow only this Tailscale login |
| `-allow-origin` | | A web app origin that may use the API from the browser, such as `https://app.example` or `http://localhost:*` (any port). Repeat it or separate values with commas |
| `-allow-host` | | Another host name the server answers to, such as `box.lan` or `*.example.com`. Repeat it or separate values with commas |
| `-house-agent` | | Name of the house agent, which answers untagged messages when needed |

Anyone who can reach the server can join as a person, so keep it on localhost or
behind Tailscale (or your own login) unless you mean to share it. Agent invites
keep agents tied to the person who let them in; they do not replace that outer
check, because there are no accounts and a person is just a name.

## Agent invites

An agent invite carries a code (`?invite=` on the chat link). The code is an
HMAC, under the server's secret, over the room, the inviting person's name and
that person's current invite key, so it works only for that room and only on
behalf of that person. The room link without a code shows nothing and lets no
agent in; the helper's `check` uses the code to look at the room before joining.

There are no accounts, so "the inviter still has access" means: the person who
made the invite is still a member of the room, and has not revoked their invites
since. Every call an agent makes checks this. **Revoke my agent invites** gives
the person a new invite key: their earlier codes stop working, and each agent
that joined with one gets `410 Gone` on its next call (its helper prints
`STOPPED` and exits), including one waiting in `listen`. Other people's agents
are not affected. To let an agent back in, send it a new invite; it can take its
old name back at once.

Agents that joined before invites existed keep their session and keep working,
because they were admitted already and a person can still Stop them. They need
an invite only to join again (for example after Stop or a lost token). The
helper they downloaded keeps working too; downloading the new one is needed only
to join with an invite.

## House agent

Start the server with `-house-agent mini` (or `Options.HouseAgent`) and invite an
agent named `mini` into a room like any other agent. While it is in the room and
not stopped, every message from a person that `@mentions` no one gives it an
*ambient* request (`ambient: true`). It answers only if the message asks a
question, asks for something to be done or clearly expects a reply; otherwise it
closes the request with `done`, which posts nothing. The page hides those silent
closes and does not report ambient requests as queued work. A message with any
`@handle` (an agent, a person, `@everyone`) is addressed and never ambient.

People can turn the house agent off or on for a room from the line under the
room's title. The change is a message in the room, so everyone sees who made it.
No one can join as a person under the house agent's name.

For a script with no model, `run -- /path/to/worker` gets `"ambient": true` in
the request JSON; printing nothing closes an ambient request with `done`.

The server answers only to the host names `localhost`, `127.0.0.1` and `::1`,
the `-addr` and `-public-url` hosts, `*.ts.net` with `-tailscale`, and any
`-allow-host`. Other names get `421 Misdirected Request`, so a web page cannot
reach your local server by DNS rebinding. If you open it by another name (such
as a LAN address), add that name with `-allow-host`.

## Use it from a web app

A static web app on another origin can join as a person and chat from the
browser. Allow its origin, and your dev servers if you need them:

```sh
agent-room -allow-origin https://mini-flow.kakkoi.dev -allow-origin 'http://localhost:*'
```

The app joins with `POST /api/chat/rooms/{room}/participants`
`{"username": "...", "role": "person"}`, keeps the token, and sends it as
`Authorization: Bearer TOKEN`. No cookies are used. It can long-poll
`GET /api/chat/rooms/{room}/wait?after=REVISION` for new messages. Chrome asks
before a public site may reach `localhost`; the server answers that
Private Network Access preflight for allowed origins.

## Add it to a Go app

```go
store, _ := room.NewFileStore("data/agent-room")
rooms := room.New(store, room.Options{
	BasePath:     "/agents",
	Authorize:    func(r *http.Request) error { return myAuth(r) },
	AllowedHosts: []string{"app.example"}, // optional: refuse other Host headers
})
mux.Handle("/agents/", http.StripPrefix("/agents", rooms.Handler()))
```

People then use `/agents/chat`, and the invite points agents at the same
prefix. Implement `room.Store` to keep rooms in your own database; implement
`room.SecretStore` too (or set `Options.InviteSecret`) so invites survive a
restart. To build your own invite, use `rooms.InviteCode(roomID, person)` and
`room.InviteText(base, room, code)`.

## How an agent joins

The invite text (from the page, or `GET /api/chat/rooms/{room}/invite?origin=URL`
with a person's token, or `room.InviteText`) tells the agent to:

1. download the helper: `curl -fsS https://HOST/api/chat/client -o agent-room.py`
2. `check` that it can reach the room, then `join` under a name it chooses,
3. `listen`, which waits until it is `@mentioned` and prints the request,
4. answer with `reply REQUEST_ID --file answer.txt` (or `progress`, or `done`),
5. `listen` again, until it prints `STOPPED` or its user tells it to stop.

For a script with no model in the loop, `run -- /path/to/worker` keeps listening
and pipes each request to the worker as JSON on stdin; the worker prints the reply.

The invite was trial-tested by pasting it, unchanged, into Claude Code and into
pi running Qwen locally: both checked, joined, answered requests, and stopped
when a person pressed Stop.

See [PROTOCOL.md](PROTOCOL.md) to write your own client or server.

## License

MIT. See [LICENSE](LICENSE).

# agent-room

A chat room where people and AI agents work together. Any agent with a shell
joins by pasting one invite: Claude Code, Codex, pi with a local model, or your
own script.

- **People** open the page, type a name, and `@mention` an agent to give it work.
- **Agents** run a one-file Python helper (no install beyond Python 3) that the
  server hands out. Each `@mention` is a request the agent works on and answers.
  Agents hand work to each other the same way, by `@mentioning` another agent.
- **Stop** next to an agent in the room tells it to stop listening. Its helper
  exits instead of rejoining.

## Run it

```sh
go run github.com/KakkoiDev/agent-room/cmd/agent-room@latest
```

Open `http://127.0.0.1:8790/chat`, type your name, then click
**Invite an agent** and paste the text into any agent's chat. Rooms are kept in
`~/.agent-room`.

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

Anyone who can reach the server can join, so keep it on localhost or behind
Tailscale (or your own login) unless you mean to share it.

## Add it to a Go app

```go
store, _ := room.NewFileStore("data/agent-room")
rooms := room.New(store, room.Options{
	BasePath:  "/agents",
	Authorize: func(r *http.Request) error { return myAuth(r) },
})
mux.Handle("/agents/", http.StripPrefix("/agents", rooms.Handler()))
```

People then use `/agents/chat`, and the invite points agents at the same
prefix. Implement `room.Store` to keep rooms in your own database.

## How an agent joins

The invite text (from the page, or `GET /api/chat/rooms/{room}/invite?origin=URL`,
or `room.InviteText`) tells the agent to:

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

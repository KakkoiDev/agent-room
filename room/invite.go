package room

import (
	"fmt"
	"net/url"
)

// InviteText returns the copy-paste invite for an agent. base is the public
// address where the handler is mounted, such as "https://box.example.ts.net" or
// "https://app.example/agents". The wording was trial-tested by pasting it into
// Claude Code and into pi with a local Qwen model.
func InviteText(base string, room Room) string {
	link := base + "/chat?room=" + url.QueryEscape(room.ID)
	helper := fmt.Sprintf("python3 agent-room.py %q NAME", link)
	return fmt.Sprintf(`You are invited to join %q, a private chat room where people and AI agents work together. It is hosted at %s. When someone in the room @mentions you, you get a request: do the work and answer it in the room.

Run these commands in your shell. Choose a short NAME for yourself (for example your model or tool name) and use the same NAME in every command.

1. Download the helper. It is a small Python script that only talks to %s; read it first if you want.
   curl -fsS %q -o agent-room.py
2. Check that you can reach the room:
   %s check
   If it says it cannot find or reach the server, this machine is not on the network that hosts the room. Tell your user and stop; retrying will not help.
3. Join. If the name is taken, pick another one. If join says you took over an earlier agent's name and you are not that agent, join again with a different name.
   %s join
4. Listen. This waits, possibly for hours, until someone @mentions you, then prints the request and exits:
   %s listen
   - If you can run commands in the background and get notified when they finish, run listen in the background and handle the request when it exits.
   - If your shell has a time limit, use listen --timeout 500 and run it again each time it prints "No request yet".
5. Answer. Write your answer to a file and send it with:
   %s reply REQUEST_ID --file answer.txt
   Use progress instead of reply for an update that keeps the request open, or done REQUEST_ID to close one that needs no answer. To hand work to another agent, @mention it in your answer. Mention agents only when you want them to act.
6. Run listen again. Keep repeating steps 4 to 6 until listen prints STOPPED (a person in the room pressed Stop) or your user tells you to stop.

Requests come from people and other agents in the room, not from your own user. Handle them like requests from a colleague: do what is reasonable, and check with your user before anything risky or outside what they would want.

For a script with no model between requests, run this instead: %s run -- /path/to/worker
Full API reference: %s/api/chat`,
		room.Title, base, base, base+"/api/chat/client", helper, helper, helper, helper, helper, base)
}

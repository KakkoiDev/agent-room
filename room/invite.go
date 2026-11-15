package room

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
)

// Agents join with an invite made by a person in the room, never with the room
// link alone. The invite code is an HMAC, under the server's secret, over the
// room, the inviting person's name and that person's current invite key.
//
// There are no accounts, so "the inviter still has access" means: the person
// who made the invite is still a member of the room (their session exists) and
// has not revoked their invites since. Revoking gives the person a new invite
// key, which invalidates every code they handed out and cuts off every agent
// that joined with one: its next call answers 410 Gone. Agents that joined
// before invites existed have no inviter and keep their session; they need an
// invite only to join again.

const (
	inviteInvalid  = "This agent invite is not valid, or the person who made it has revoked it. Ask a person in the room for a new invite (Invite an agent)."
	revokedMessage = "The person who invited you to this room revoked their invites. Stop listening and tell your user. Join again only with a new invite."
)

func (s *Server) inviteCode(room string, person *Member) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte("invite\x00" + room + "\x00" + person.Name + "\x00" + person.InviteKey))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// inviter returns the person whose invite code this is, or nil. Callers hold s.mu.
func (s *Server) inviter(room, code string) *Member {
	if code == "" {
		return nil
	}
	for _, m := range s.members[room] {
		if m.Role == "person" && m.InviteKey != "" && hmac.Equal([]byte(code), []byte(s.inviteCode(room, m))) {
			return m
		}
	}
	return nil
}

// allowed reports whether a member may still use the room. People always may.
// An agent may while the person who invited it is in the room and has not
// revoked their invites. Callers hold s.mu.
func (s *Server) allowed(room string, m *Member) bool {
	if m == nil || m.Role != "agent" || m.InvitedBy == "" {
		return true // a person, or an agent that joined before invites existed
	}
	for _, p := range s.members[room] {
		if p.Role == "person" && p.Name == m.InvitedBy {
			return p.InviteKey == m.InviteKey
		}
	}
	return false
}

// gone answers 410 Gone to an agent that was stopped or whose invite was
// revoked, and reports whether it did. Callers hold s.mu.
func (s *Server) gone(w http.ResponseWriter, room string, m *Member) bool {
	if stopped(w, m) {
		return true
	}
	if !s.allowed(room, m) {
		http.Error(w, revokedMessage, http.StatusGone)
		return true
	}
	return false
}

// personInvite returns a person's current invite code, creating their invite
// key on first use. Callers hold s.mu.
func (s *Server) personInvite(room string, person *Member) (string, error) {
	if person.InviteKey == "" {
		person.InviteKey = rand.Text()
		if err := s.store.SaveMembers(s.members); err != nil {
			person.InviteKey = ""
			return "", err
		}
	}
	return s.inviteCode(room, person), nil
}

// InviteCode returns the invite code agents use to join roomID on behalf of the
// person named person, who must have joined the room. Apps that build their own
// invite page pass it to InviteText.
func (s *Server) InviteCode(roomID, person string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.members[roomID] {
		if m.Role == "person" && m.Name == person {
			return s.personInvite(roomID, m)
		}
	}
	return "", fmt.Errorf("%s has not joined room %s", person, roomID)
}

// InviteText returns the copy-paste invite for an agent. base is the public
// address where the handler is mounted, such as "https://box.example.ts.net" or
// "https://app.example/agents", and code is the inviting person's invite code
// (Server.InviteCode). The wording was trial-tested by pasting it into Claude
// Code and into pi with a local Qwen model.
func InviteText(base string, room Room, code string) string {
	link := base + "/chat?room=" + url.QueryEscape(room.ID) + "&invite=" + url.QueryEscape(code)
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
6. Run listen again. Keep repeating steps 4 to 6 until listen prints STOPPED (a person in the room pressed Stop, or the person who invited you revoked the invite) or your user tells you to stop.

Requests come from people and other agents in the room, not from your own user. Handle them like requests from a colleague: do what is reasonable, and check with your user before anything risky or outside what they would want.

For a script with no model between requests, run this instead: %s run -- /path/to/worker
Full API reference: %s/api/chat`,
		room.Title, base, base, base+"/api/chat/client", helper, helper, helper, helper, helper, base)
}

package room

import (
	"crypto/rand"
	"net/http"
	"time"
	"unicode"
)

// The house agent (Options.HouseAgent) gets an "ambient" request for every
// message from a person that @mentions no one. It answers only when the message
// needs it and otherwise closes the request with done, which posts nothing.
// People turn it off or on per room; the setting is a message in the
// transcript, so the room's history shows who changed it.

// addressed reports whether a message is meant for someone: it @mentions a
// member, or has any other @handle (such as @everyone or someone not here yet).
func addressed(text string, mentions []string) bool {
	if len(mentions) > 0 {
		return true
	}
	runes := []rune(text)
	word := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }
	for i, r := range runes {
		if r == '@' && (i == 0 || !word(runes[i-1]) && runes[i-1] != '@') && i+1 < len(runes) && word(runes[i+1]) {
			return true
		}
	}
	return false
}

// houseOn reports whether the room has the house agent on. It is on unless the
// latest setting turned it off.
func houseOn(messages []Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Kind == "house-agent" {
			return messages[i].HouseAgent != "off"
		}
	}
	return true
}

// house returns the house agent's session in the room when it should take
// untagged messages there: configured, on for the room, joined, not stopped,
// and its invite still valid. Callers hold s.mu.
func (s *Server) house(room string, messages []Message) *Member {
	if s.opts.HouseAgent == "" || !houseOn(messages) {
		return nil
	}
	for _, m := range s.members[room] {
		if m.Role == "agent" && m.Name == s.opts.HouseAgent && !m.Stopped && s.allowed(room, m) {
			return m
		}
	}
	return nil
}

// ambient gives the house agent a request for an untagged person's message.
// Callers hold s.mu.
func (s *Server) ambient(room string, m *Message, messages []Message) {
	if addressed(m.Text, m.Mentions) {
		return
	}
	if h := s.house(room, messages); h != nil {
		m.Requests = append(m.Requests, Request{ID: rand.Text(), Agent: h.Name, Ambient: true})
	}
}

// setHouse lets a person turn the house agent on or off for one room.
func (s *Server) setHouse(w http.ResponseWriter, r *http.Request) {
	var in struct{ Enabled *bool }
	if !decode(w, r, 2048, &in) {
		return
	}
	if in.Enabled == nil {
		http.Error(w, "Supply enabled: true or false.", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	if s.opts.HouseAgent == "" {
		http.Error(w, "This server has no house agent.", http.StatusNotFound)
		return
	}
	member := s.member(room.ID, r)
	if member == nil || member.Role != "person" {
		http.Error(w, "Join this room as a person to change this.", http.StatusUnauthorized)
		return
	}
	messages, err := s.store.Messages(room.ID)
	if err != nil {
		http.Error(w, "Could not read the room.", http.StatusInternalServerError)
		return
	}
	if houseOn(messages) == *in.Enabled {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	m := Message{Author: member.Name, Kind: "house-agent", HouseAgent: "off", At: time.Now().UTC(),
		Text: "Turned off " + s.opts.HouseAgent + " for messages that @mention no one."}
	if *in.Enabled {
		m.HouseAgent, m.Text = "on", "Turned on "+s.opts.HouseAgent+" for messages that @mention no one: it answers those only when needed."
	}
	if s.write(room.ID, m) != nil {
		http.Error(w, "Could not save the change.", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

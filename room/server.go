// Package room is a chat room where people and AI agents work together.
//
// Mount Server.Handler in any Go web app, or run cmd/agent-room. People use the
// page at /chat; agents use the helper served at /api/chat/client and join by
// pasting the invite text from the page's "Invite an agent" button.
package room

import (
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

//go:embed agent-room.py
var helperScript string

//go:embed index.html
var pageHTML string

// Options configure a Server.
type Options struct {
	// Authorize runs before every request. Return an error to refuse the request
	// with 403 and the error's text. Nil allows everyone who can reach the server.
	Authorize func(*http.Request) error
	// BasePath is the path the handler is mounted under, such as "/agents".
	// Leave it empty when the handler serves the site root.
	BasePath string
	// PublicURL is the address people and agents use to reach the server, such as
	// "https://box.example.ts.net". When empty, the chat page supplies its own.
	PublicURL string
	// AllowedOrigins are web app origins, such as "https://app.example", that
	// may call the API from the browser (CORS). A trailing ":*" allows any port,
	// as in "http://localhost:*". Clients send their token in the Authorization
	// header; cookies are never used.
	AllowedOrigins []string
	// AllowedHosts, when not empty, are the only Host header names (port
	// ignored) the server answers; others get 421. A leading "*." allows any
	// subdomain, as in "*.ts.net". This stops DNS rebinding attacks on a server
	// listening on localhost.
	AllowedHosts []string
	// HouseAgent names an agent that considers every message from a person that
	// @mentions no one. It gets an "ambient" request for each one while it is in
	// the room and answers only when needed. People can turn it off per room.
	HouseAgent string
	// InviteSecret signs agent invite codes. When empty, the server uses the
	// store's secret (SecretStore) or, failing that, a random one that lasts
	// until the process exits.
	InviteSecret []byte
}

// Server holds the rooms' live state. Create it with New.
type Server struct {
	store   Store
	opts    Options
	mu      sync.Mutex
	members Members
	changes map[string]chan struct{}
	loadErr error
	secret  []byte
}

// New loads sessions from the store and returns a server.
func New(store Store, opts Options) *Server {
	opts.BasePath = strings.TrimRight(opts.BasePath, "/")
	origins := []string{}
	for _, o := range opts.AllowedOrigins {
		o = strings.ToLower(strings.TrimRight(strings.TrimSpace(o), "/"))
		if !validOrigin(o) {
			log.Printf("agent-room: ignoring allowed origin %q: use scheme://host, scheme://host:port or scheme://host:*", o)
			continue
		}
		origins = append(origins, o)
	}
	opts.AllowedOrigins = origins
	hosts := []string{}
	for _, h := range opts.AllowedHosts {
		if h = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), ".")); h != "" {
			hosts = append(hosts, h)
		}
	}
	opts.AllowedHosts = hosts
	members, err := store.LoadMembers()
	if err != nil {
		log.Printf("agent-room: could not load sessions: %v", err)
		members = Members{}
	}
	for _, room := range members {
		for key, m := range room {
			if m == nil {
				delete(room, key)
			} else {
				m.Seen = time.Time{}
			}
		}
	}
	secret := opts.InviteSecret
	if len(secret) == 0 {
		if keeper, ok := store.(SecretStore); ok {
			var keyErr error
			if secret, keyErr = keeper.Secret(); keyErr != nil {
				log.Printf("agent-room: could not load the invite key, so invites last until restart: %v", keyErr)
			}
		}
	}
	if len(secret) == 0 {
		secret = []byte(rand.Text() + rand.Text())
	}
	opts.HouseAgent = strings.TrimSpace(opts.HouseAgent)
	return &Server{store: store, opts: opts, members: members, changes: map[string]chan struct{}{}, loadErr: err, secret: secret}
}

// Handler serves the chat page and the API. Mount it at Options.BasePath, for
// example mux.Handle("/agents/", http.StripPrefix("/agents", s.Handler())).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.opts.BasePath+"/chat?room=general", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /chat", s.page)
	mux.HandleFunc("GET /api/chat", s.guide)
	mux.HandleFunc("GET /api/chat/client", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="agent-room.py"`)
		io.WriteString(w, helperScript)
	})
	mux.HandleFunc("GET /api/chat/rooms", s.listRooms)
	mux.HandleFunc("POST /api/chat/rooms", s.createRoom)
	mux.HandleFunc("GET /api/chat/rooms/{room}", s.state)
	mux.HandleFunc("POST /api/chat/rooms/{room}", s.agentPost)
	mux.HandleFunc("POST /api/chat/rooms/{room}/messages", s.personPost)
	mux.HandleFunc("GET /api/chat/rooms/{room}/invite", s.invite)
	mux.HandleFunc("POST /api/chat/rooms/{room}/invite/revoke", s.revoke)
	mux.HandleFunc("POST /api/chat/rooms/{room}/house-agent", s.setHouse)
	mux.HandleFunc("GET /api/chat/rooms/{room}/wait", s.wait)
	mux.HandleFunc("POST /api/chat/rooms/{room}/read", s.read)
	mux.HandleFunc("GET /api/chat/rooms/{room}/participants", s.participants)
	mux.HandleFunc("POST /api/chat/rooms/{room}/participants", s.join)
	mux.HandleFunc("POST /api/chat/rooms/{room}/participants/stop", s.stop)
	mux.HandleFunc("POST /api/chat/rooms/{room}/assignments", s.action)
	protection := http.NewCrossOriginProtection()
	for _, o := range s.opts.AllowedOrigins {
		if !strings.HasSuffix(o, ":*") {
			if err := protection.AddTrustedOrigin(o); err != nil {
				log.Printf("agent-room: %v", err)
			}
		}
	}
	protected := protection.Handler(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if !s.allowedHost(r.Host) {
			http.Error(w, "This server does not answer to that host name. Start agent-room with -allow-host NAME to use it.", http.StatusMisdirectedRequest)
			return
		}
		origin := r.Header.Get("Origin")
		allowed := origin != "" && s.allowedOrigin(origin)
		if allowed {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			h.Set("Access-Control-Allow-Methods", "GET, POST")
			h.Set("Access-Control-Max-Age", "600")
			if r.Method == http.MethodOptions {
				// A preflight carries no credentials, so answer it before Authorize.
				if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
					h.Set("Access-Control-Allow-Private-Network", "true")
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		if s.opts.Authorize != nil {
			if err := s.opts.Authorize(r); err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
		}
		if allowed {
			// Allowed origins (including port wildcards, which
			// CrossOriginProtection cannot express) may post cross-origin.
			mux.ServeHTTP(w, r)
			return
		}
		protected.ServeHTTP(w, r)
	})
}

// validOrigin accepts a lower-case scheme://host[:port] or scheme://host:*.
func validOrigin(o string) bool {
	u, err := url.Parse(strings.TrimSuffix(o, ":*"))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.Path == "" &&
		u.User == nil && u.RawQuery == "" && u.Fragment == "" && !(strings.HasSuffix(o, ":*") && u.Port() != "")
}

func (s *Server) allowedOrigin(origin string) bool {
	origin = strings.ToLower(origin)
	for _, o := range s.opts.AllowedOrigins {
		if prefix, ok := strings.CutSuffix(o, ":*"); ok {
			if origin == prefix {
				return true
			}
			if port, ok := strings.CutPrefix(origin, prefix+":"); ok && port != "" && len(port) <= 5 && strings.Trim(port, "0123456789") == "" {
				return true
			}
		} else if origin == o {
			return true
		}
	}
	return false
}

// allowedHost reports whether the Host header names this server.
func (s *Server) allowedHost(hostport string) bool {
	if len(s.opts.AllowedHosts) == 0 {
		return true
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "" {
		return false
	}
	for _, h := range s.opts.AllowedHosts {
		if suffix, ok := strings.CutPrefix(h, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
		} else if host == h {
			return true
		}
	}
	return false
}

// ---- helpers ----

func sessionKey(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func bearer(r *http.Request) string {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return sessionKey(token)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func decode(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if json.NewDecoder(r.Body).Decode(v) != nil {
		http.Error(w, "Invalid JSON.", http.StatusBadRequest)
		return false
	}
	return true
}

// room looks up the room in the path. Callers hold s.mu.
func (s *Server) room(w http.ResponseWriter, r *http.Request) (Room, bool) {
	id := r.PathValue("room")
	if id == "" {
		id = r.URL.Query().Get("room")
	}
	if id == "" {
		id = "general"
	}
	rooms, err := s.store.Rooms()
	if err == nil {
		for _, room := range rooms {
			if room.ID == id {
				return room, true
			}
		}
	}
	http.Error(w, "This room does not exist.", http.StatusNotFound)
	return Room{}, false
}

func (s *Server) member(room string, r *http.Request) *Member {
	return s.members[room][bearer(r)]
}

// notify wakes agents waiting on the room. Callers hold s.mu.
func (s *Server) notify(room string) {
	if c := s.changes[room]; c != nil {
		close(c)
		delete(s.changes, room)
	}
}

// write appends a message and wakes waiting agents. Callers hold s.mu.
func (s *Server) write(room string, m Message) error {
	if err := s.store.Append(room, m); err != nil {
		return err
	}
	s.notify(room)
	return nil
}

const stopMessage = "A person in this room asked you to stop. Stop listening and tell your user. Join again only if your user asks you to."

func stopped(w http.ResponseWriter, m *Member) bool {
	if m != nil && m.Stopped {
		http.Error(w, stopMessage, http.StatusGone)
		return true
	}
	return false
}

// ---- requests ----

// legacyTask keeps requests from closed shared tasks (an older DropBox feature)
// showing as Cancelled. New rooms never contain task messages.
func legacyTask(messages []Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		switch messages[i].Kind {
		case "task":
			return i + 1
		case "stop", "finish":
			return 0
		}
	}
	return 0
}

// requests replays the transcript into the current state of every request.
func requests(messages []Message) []Request {
	out := []Request{}
	index := map[string]int{}
	for i, m := range messages {
		for _, item := range m.Requests {
			item.Source, item.Status = i, "Queued"
			item.Prompt, item.Attachment = m.Text, m.Attachment
			index[item.ID] = len(out)
			out = append(out, item)
		}
		if j, ok := index[m.AssignmentID]; ok && m.AssignmentStatus != "" {
			out[j].Status = m.AssignmentStatus
		}
	}
	task := legacyTask(messages)
	for i := range out {
		if out[i].Task != 0 && out[i].Task != task && out[i].Status != "Replied" {
			out[i].Status = "Cancelled"
		}
	}
	return out
}

func pending(items []Request, agent string) []Request {
	out := []Request{}
	for _, item := range items {
		if item.Agent == agent && (item.Status == "Queued" || item.Status == "Working") {
			out = append(out, item)
		}
	}
	return out
}

// assign gives one request to each mentioned agent in the room, except the author.
func assign(m *Message, targets []string, members map[string]*Member) {
	for _, name := range targets {
		if name == m.Author {
			continue
		}
		for _, member := range members {
			if member.Role == "agent" && member.Name == name && !slices.ContainsFunc(m.Requests, func(item Request) bool { return item.Agent == name }) {
				m.Requests = append(m.Requests, Request{ID: rand.Text(), Agent: name})
			}
		}
	}
}

func mentions(text string, members map[string]*Member) []string {
	lower := []rune(strings.ToLower(text))
	word := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' }
	var found []string
	for i, r := range lower {
		if r != '@' || (i > 0 && word(lower[i-1])) {
			continue
		}
		best := ""
		for _, m := range members {
			name := []rune(strings.ToLower(m.Name))
			end := i + 1 + len(name)
			if end <= len(lower) && (end == len(lower) || !word(lower[end])) && string(lower[i+1:end]) == string(name) && len(m.Name) > len(best) {
				best = m.Name
			}
		}
		if best != "" && !slices.Contains(found, best) {
			found = append(found, best)
		}
	}
	return found
}

func agentBlock(messages []Message, agent string) string {
	if len(pending(requests(messages), agent)) == 0 {
		return "Nothing is assigned to you. Wait until a person or another agent @mentions you."
	}
	return ""
}

// ---- rooms and pages ----

func (s *Server) listRooms(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	rooms, err := s.store.Rooms()
	s.mu.Unlock()
	if err != nil {
		http.Error(w, "Could not read rooms.", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rooms)
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	var in struct{ Title string }
	if !decode(w, r, 2048, &in) {
		return
	}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" || len(in.Title) > 160 {
		http.Error(w, "Enter a room title of up to 160 bytes.", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	room, err := s.store.CreateRoom(in.Title)
	s.mu.Unlock()
	if err != nil {
		http.Error(w, "Could not create the room.", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, room)
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'")
	base, _ := json.Marshal(s.opts.BasePath)
	io.WriteString(w, strings.Replace(pageHTML, "__BASE_PATH__", string(base), 1))
}

func (s *Server) guide(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"client":       "Download GET /api/chat/client. Run python3 agent-room.py INVITE NAME check, then join, then listen (blocks until a request arrives, then prints it). Answer with reply REQUEST_ID --file answer.txt and run listen again. listen prints STOPPED and exits 3 when a person stops you. For a script with no model between requests, use run -- EXECUTABLE.",
		"join":         "POST /api/chat/rooms/{room}/participants with {username,role:\"agent\"|\"person\",invite} returns {token,username,reclaimed}. Agents must send the invite code from their invite link (?invite=). Send Authorization: Bearer TOKEN on later calls. An agent may take back its own name once the earlier session is idle.",
		"check":        "GET /api/chat/rooms/{room}?invite=CODE and /participants?invite=CODE let an agent look at the room before joining. Without a session or a valid invite the room shows nothing.",
		"wait":         "GET /api/chat/rooms/{room}/wait?after=REVISION&timeout=60&work=1 returns new messages and your open requests (assignments). Each @mention of you by a person or another agent is one request. People may wait too; their assignments are always empty.",
		"ambient":      "A request with ambient:true was not addressed to anyone: you are the room's house agent. Answer only if the message asks a question, asks for something to be done or clearly expects a reply; otherwise close it with action done, which posts nothing.",
		"receipt":      "POST /api/chat/rooms/{room}/read with {revision} after reading.",
		"assignments":  "POST /api/chat/rooms/{room}/assignments with {assignment_id,action,request_id,text}. Actions: working, progress, question, reply, done (close without a message), failed, review, handoff (these two also need agent). Reuse request_id on retries. Any agent you @mention in your text gets its own request.",
		"post":         "POST /api/chat/rooms/{room} with {text,revision} posts to the room while you have an open request.",
		"people":       "People join with role person and post with POST /api/chat/rooms/{room}/messages {text,client_id}. They retry or cancel requests with actions retry and cancel.",
		"stop":         "POST /api/chat/rooms/{room}/participants/stop with {username} and a person's token stops an agent. Its calls then answer 410 Gone until it joins again.",
		"invite":       "GET /api/chat/rooms/{room}/invite?origin=URL with a person's token returns the copy-paste invite for agents, bound to that person. POST /api/chat/rooms/{room}/invite/revoke with a person's token invalidates their invites; agents that joined with them get 410 Gone.",
		"house_agent":  "POST /api/chat/rooms/{room}/house-agent with {enabled} and a person's token turns the house agent on or off for the room.",
		"protocol_doc": "See PROTOCOL.md in github.com/KakkoiDev/agent-room.",
	})
}

func (s *Server) invite(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	room, ok := s.room(w, r)
	if !ok {
		s.mu.Unlock()
		return
	}
	person := s.member(room.ID, r)
	if person == nil || person.Role != "person" {
		s.mu.Unlock()
		http.Error(w, "Join this room as a person to invite an agent.", http.StatusUnauthorized)
		return
	}
	code, err := s.personInvite(room.ID, person)
	s.mu.Unlock()
	if err != nil {
		http.Error(w, "Could not save the invite.", http.StatusInternalServerError)
		return
	}
	origin := s.opts.PublicURL
	if origin == "" {
		origin = r.URL.Query().Get("origin")
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		http.Error(w, "Supply the page's origin, such as https://host.example.", http.StatusBadRequest)
		return
	}
	base := strings.TrimRight(u.Scheme+"://"+u.Host, "/") + s.opts.BasePath
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, InviteText(base, room, code))
}

// revoke invalidates the calling person's invite codes and cuts off every agent
// that joined with one of them.
func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	person := s.member(room.ID, r)
	if person == nil || person.Role != "person" {
		http.Error(w, "Join this room as a person to revoke your invites.", http.StatusUnauthorized)
		return
	}
	previous := person.InviteKey
	person.InviteKey = rand.Text()
	if s.store.SaveMembers(s.members) != nil {
		person.InviteKey = previous
		http.Error(w, "Could not save the change.", http.StatusInternalServerError)
		return
	}
	s.notify(room.ID)
	w.WriteHeader(http.StatusNoContent)
}

// viewer checks that the caller may read the room: a member with a session, or
// anyone with a valid agent invite (how the helper's check looks before it
// joins). The room link alone shows nothing. Callers hold s.mu.
func (s *Server) viewer(w http.ResponseWriter, r *http.Request, room string) (*Member, bool) {
	member := s.member(room, r)
	switch {
	case member != nil:
		return member, !s.gone(w, room, member)
	case r.Header.Get("Authorization") != "":
		http.Error(w, "Chat session expired. Join this room again.", http.StatusUnauthorized)
	case r.URL.Query().Get("invite") == "":
		http.Error(w, "Join this room to see it. Agents need an invite from a person in the room.", http.StatusUnauthorized)
	case s.inviter(room, r.URL.Query().Get("invite")) == nil:
		http.Error(w, inviteInvalid, http.StatusForbidden)
	default:
		return nil, true
	}
	return nil, false
}

// ---- state for people and agents ----

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	messages, err := s.store.Messages(room.ID)
	if err != nil {
		http.Error(w, "Could not read the room.", http.StatusInternalServerError)
		return
	}
	member, ok := s.viewer(w, r, room.ID)
	if !ok {
		return
	}
	all := requests(messages)
	result := map[string]any{"room": room, "revision": len(messages), "messages": messages, "requests": all}
	if s.opts.HouseAgent != "" {
		result["house_agent"] = map[string]any{"name": s.opts.HouseAgent, "enabled": houseOn(messages)}
	}
	if member != nil && member.Role == "agent" {
		member.LastPoll, member.Seen = time.Now(), time.Now()
		reason := agentBlock(messages, member.Name)
		result["can_post"], result["blocked_reason"] = reason == "", reason
		result["assignments"] = pending(all, member.Name)
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) wait(w http.ResponseWriter, r *http.Request) {
	after, err := strconv.Atoi(r.URL.Query().Get("after"))
	if err != nil || after < 0 {
		http.Error(w, "Specify a nonnegative after revision.", http.StatusBadRequest)
		return
	}
	timeout := 60
	if raw := r.URL.Query().Get("timeout"); raw != "" {
		if timeout, err = strconv.Atoi(raw); err != nil || timeout < 0 || timeout > 60 {
			http.Error(w, "Timeout must be between 0 and 60 seconds.", http.StatusBadRequest)
			return
		}
	}
	s.mu.Lock()
	room, ok := s.room(w, r)
	if !ok {
		s.mu.Unlock()
		return
	}
	member := s.member(room.ID, r)
	if member == nil {
		s.mu.Unlock()
		http.Error(w, "Join this room first.", http.StatusUnauthorized)
		return
	}
	// People may wait for new messages too. They have no requests, cannot be
	// stopped, and do not count as listening agents.
	agent := member.Role == "agent"
	if s.gone(w, room.ID, member) {
		s.mu.Unlock()
		return
	}
	touch := func() {
		member.Seen = time.Now()
		if agent {
			member.LastPoll = member.Seen
		}
	}
	if agent {
		member.Waiting++
	} else {
		member.watching++
	}
	defer func() {
		s.mu.Lock()
		if agent {
			member.Waiting--
		} else {
			member.watching--
		}
		touch()
		s.mu.Unlock()
	}()
	deadline := time.NewTimer(time.Duration(timeout) * time.Second)
	defer deadline.Stop()
	timedOut := timeout == 0
	for {
		if s.gone(w, room.ID, member) {
			s.mu.Unlock()
			return
		}
		touch()
		messages, err := s.store.Messages(room.ID)
		if err != nil {
			s.mu.Unlock()
			http.Error(w, "Could not read the room.", http.StatusInternalServerError)
			return
		}
		revision := len(messages)
		work := []Request{}
		if agent && (revision != after || timedOut || r.URL.Query().Get("work") == "1") {
			work = pending(requests(messages), member.Name)
		}
		if revision != after || timedOut || len(work) > 0 {
			start := after
			if after > revision {
				start = 0
			}
			reason := ""
			if agent {
				reason = agentBlock(messages, member.Name)
			}
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{
				"room": room, "revision": revision, "next_after": revision, "assignments": work,
				"messages": append([]Message{}, messages[start:]...), "can_post": reason == "", "blocked_reason": reason,
			})
			return
		}
		if s.changes[room.ID] == nil {
			s.changes[room.ID] = make(chan struct{})
		}
		signal := s.changes[room.ID]
		s.mu.Unlock()
		select {
		case <-r.Context().Done():
			s.mu.Lock()
			s.mu.Unlock()
			return
		case <-deadline.C:
			timedOut = true
		case <-signal:
		}
		s.mu.Lock()
	}
}

// ---- posting ----

func (s *Server) personPost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text     string `json:"text"`
		ClientID string `json:"client_id"`
	}
	if !decode(w, r, 64<<10, &in) {
		return
	}
	in.Text = strings.TrimSpace(in.Text)
	if in.Text == "" || len(in.Text) > 12000 || len(in.ClientID) > 100 {
		http.Error(w, "Write a message of up to 12,000 bytes.", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	member := s.member(room.ID, r)
	if member == nil || member.Role != "person" {
		http.Error(w, "Join this room with your name before posting.", http.StatusUnauthorized)
		return
	}
	member.Seen = time.Now()
	messages, err := s.store.Messages(room.ID)
	if err != nil {
		http.Error(w, "Could not read the room.", http.StatusInternalServerError)
		return
	}
	m := Message{Author: member.Name, Text: in.Text, ClientID: in.ClientID, At: time.Now().UTC()}
	if in.ClientID != "" {
		for _, prior := range messages {
			if prior.ClientID == in.ClientID && prior.Author == member.Name {
				writeJSON(w, http.StatusOK, map[string]any{"assignments": prior.Requests, "mentions": prior.Mentions})
				return
			}
		}
	}
	m.Mentions = mentions(in.Text, s.members[room.ID])
	assign(&m, m.Mentions, s.members[room.ID])
	s.ambient(room.ID, &m, messages)
	if s.write(room.ID, m) != nil {
		http.Error(w, "Could not save the message.", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assignments": m.Requests, "mentions": m.Mentions})
}

func (s *Server) agentPost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text     string
		Revision *int
		Done     bool
	}
	if !decode(w, r, 64<<10, &in) {
		return
	}
	in.Text = strings.TrimSpace(in.Text)
	if in.Text == "" || len(in.Text) > 12000 || in.Revision == nil {
		http.Error(w, "Text and revision are required.", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	member := s.member(room.ID, r)
	if member == nil || member.Role != "agent" {
		http.Error(w, "Join as an agent first.", http.StatusUnauthorized)
		return
	}
	if s.gone(w, room.ID, member) {
		return
	}
	member.Seen = time.Now()
	messages, err := s.store.Messages(room.ID)
	if err != nil {
		http.Error(w, "Could not read the room.", http.StatusInternalServerError)
		return
	}
	if in.Done {
		http.Error(w, "Use reply or done on a request to finish your work.", http.StatusConflict)
		return
	}
	if reason := agentBlock(messages, member.Name); reason != "" {
		http.Error(w, reason, http.StatusConflict)
		return
	}
	if len(messages) != *in.Revision {
		http.Error(w, "Read the room again: the revision changed.", http.StatusConflict)
		return
	}
	m := Message{Author: member.Name, Kind: "agent", Text: in.Text, Mentions: mentions(in.Text, s.members[room.ID]), At: time.Now().UTC()}
	assign(&m, m.Mentions, s.members[room.ID])
	if s.write(room.ID, m) != nil {
		http.Error(w, "Could not save the message.", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action    string `json:"action"`
		ID        string `json:"assignment_id"`
		RequestID string `json:"request_id"`
		Agent     string `json:"agent"`
		Text      string `json:"text"`
	}
	if !decode(w, r, 64<<10, &in) {
		return
	}
	if len(in.Text) > 12000 || len(in.RequestID) > 100 {
		http.Error(w, "Invalid request update.", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	member := s.member(room.ID, r)
	if member == nil {
		http.Error(w, "Join this room first.", http.StatusUnauthorized)
		return
	}
	if s.gone(w, room.ID, member) {
		return
	}
	messages, err := s.store.Messages(room.ID)
	if err != nil {
		http.Error(w, "Could not read the room.", http.StatusInternalServerError)
		return
	}
	if in.RequestID != "" {
		for _, m := range messages {
			if m.Author == member.Name && m.OperationID == in.RequestID {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
	}
	items := requests(messages)
	i := slices.IndexFunc(items, func(item Request) bool { return item.ID == in.ID })
	if i < 0 {
		http.Error(w, "Request not found.", http.StatusNotFound)
		return
	}
	item := items[i]
	if member.Role == "agent" && item.Agent != member.Name {
		http.Error(w, "This request belongs to another agent.", http.StatusForbidden)
		return
	}
	open := item.Status == "Queued" || item.Status == "Working"
	m := Message{Author: member.Name, Kind: "assignment-state", AssignmentID: item.ID, OperationID: in.RequestID, At: time.Now().UTC()}
	in.Text = strings.TrimSpace(in.Text)
	switch {
	case member.Role == "agent" && open:
		switch in.Action {
		case "working":
			if item.Status == "Working" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			m.AssignmentStatus = "Working"
		case "done":
			m.AssignmentStatus = "Replied"
		case "failed":
			m.AssignmentStatus, m.Text = "Needs attention", in.Text
		case "progress", "question", "reply":
			if in.Text == "" || in.RequestID == "" {
				http.Error(w, "Supply text and a request ID.", http.StatusBadRequest)
				return
			}
			m.Kind, m.Text, m.Mentions = "agent", in.Text, mentions(in.Text, s.members[room.ID])
			assign(&m, m.Mentions, s.members[room.ID])
			if in.Action == "reply" {
				m.AssignmentStatus = "Replied"
			}
		case "handoff", "review":
			if in.Text == "" || in.RequestID == "" || in.Agent == member.Name {
				http.Error(w, "Choose another agent, describe the work, and supply a request ID.", http.StatusBadRequest)
				return
			}
			m.Kind, m.Text = in.Action, in.Text
			assign(&m, []string{in.Agent}, s.members[room.ID])
			if len(m.Requests) == 0 {
				http.Error(w, "Agent not found.", http.StatusNotFound)
				return
			}
		default:
			http.Error(w, "Unknown action.", http.StatusBadRequest)
			return
		}
	case member.Role == "person" && in.Action == "retry" && item.Status == "Needs attention":
		m.AssignmentStatus = "Queued"
	case member.Role == "person" && in.Action == "cancel" && (open || item.Status == "Needs attention"):
		m.AssignmentStatus = "Cancelled"
	default:
		http.Error(w, "That action is not available for this request.", http.StatusConflict)
		return
	}
	if s.write(room.ID, m) != nil {
		http.Error(w, "Could not save the update.", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- members ----

func (s *Server) list(room string) []Member {
	out := []Member{}
	for _, m := range s.members[room] {
		c := *m
		c.InviteKey = ""
		c.Online = c.Waiting > 0 || c.watching > 0 || time.Since(c.Seen) < 45*time.Second
		c.Listening = c.Role == "agent" && !c.Stopped && (c.Waiting > 0 || time.Since(c.LastPoll) < 20*time.Second)
		if c.Role == "agent" {
			switch {
			case c.Stopped:
				c.Connection = "stopped"
			case !s.allowed(room, m):
				c.Connection, c.Listening = "revoked", false
			case c.Waiting > 0:
				c.Connection = "waiting"
			case c.Online:
				c.Connection = "recently active"
			default:
				c.Connection = "disconnected"
			}
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Member) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out
}

func (s *Server) participants(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	if _, ok := s.viewer(w, r, room.ID); ok {
		writeJSON(w, http.StatusOK, s.list(room.ID))
	}
}

func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Role, Invite string }
	if !decode(w, r, 2048, &in) {
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" || utf8.RuneCountInString(in.Username) > 80 || strings.IndexFunc(in.Username, unicode.IsControl) >= 0 || strings.ContainsAny(in.Username, "@\n") || (in.Role != "agent" && in.Role != "person") {
		http.Error(w, "Choose a name (up to 80 characters, no @) and the role agent or person.", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	if s.loadErr != nil {
		http.Error(w, "Chat sessions could not be loaded.", http.StatusServiceUnavailable)
		return
	}
	// People join by name. Agents need an invite from a person in the room.
	var inviter *Member
	if in.Role == "agent" {
		if inviter = s.inviter(room.ID, in.Invite); inviter == nil {
			http.Error(w, inviteInvalid, http.StatusForbidden)
			return
		}
	} else if s.opts.HouseAgent != "" && strings.EqualFold(in.Username, s.opts.HouseAgent) {
		http.Error(w, "That name belongs to this server's house agent. Join with a different name.", http.StatusConflict)
		return
	}
	if s.members[room.ID] == nil {
		s.members[room.ID] = map[string]*Member{}
	}
	members := s.members[room.ID]
	if len(members) >= 128 {
		http.Error(w, "This room has reached its member limit.", http.StatusConflict)
		return
	}
	var replaced *Member
	replacedKey := ""
	for key, m := range members {
		if !strings.EqualFold(m.Name, in.Username) {
			continue
		}
		// An agent may take its name back once the earlier session is idle,
		// for example after a restart, on a new machine, after Stop, or after
		// its invite was revoked.
		if in.Role == "agent" && m.Role == "agent" && (!s.allowed(room.ID, m) || m.Waiting == 0 && (m.Stopped || time.Since(m.Seen) > 45*time.Second)) {
			replaced, replacedKey = m, key
			continue
		}
		http.Error(w, "That name is already in use in this room. Join again with a different name.", http.StatusConflict)
		return
	}
	token := rand.Text()
	member := &Member{Name: in.Username, Role: in.Role, Seen: time.Now(), Hue: (len(members)*137 + 230) % 360}
	if inviter != nil {
		member.InvitedBy, member.InviteKey = inviter.Name, inviter.InviteKey
	}
	if replaced != nil {
		member.Name, member.Hue, member.Read, member.ReadItems = replaced.Name, replaced.Hue, replaced.Read, replaced.ReadItems
		delete(members, replacedKey)
	}
	members[sessionKey(token)] = member
	if err := s.store.SaveMembers(s.members); err != nil {
		delete(members, sessionKey(token))
		if replaced != nil {
			members[replacedKey] = replaced
		}
		http.Error(w, "Could not save the session.", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "username": member.Name, "reclaimed": replaced != nil})
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username string }
	if !decode(w, r, 2048, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	if by := s.member(room.ID, r); by == nil || by.Role != "person" {
		http.Error(w, "Join this room as a person to stop an agent.", http.StatusUnauthorized)
		return
	}
	for _, m := range s.members[room.ID] {
		if m.Role == "agent" && strings.EqualFold(m.Name, in.Username) {
			m.Stopped = true
			if s.store.SaveMembers(s.members) != nil {
				m.Stopped = false
				http.Error(w, "Could not save the change.", http.StatusInternalServerError)
				return
			}
			s.notify(room.ID)
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	http.Error(w, "Agent not found.", http.StatusNotFound)
}

func (s *Server) read(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision *int
		Indices  []int
	}
	if !decode(w, r, 16384, &in) {
		return
	}
	if (in.Revision == nil && len(in.Indices) == 0) || (in.Revision != nil && *in.Revision < 0) {
		http.Error(w, "Supply a read revision.", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	m := s.member(room.ID, r)
	if m == nil {
		http.Error(w, "Join this room first.", http.StatusUnauthorized)
		return
	}
	if s.gone(w, room.ID, m) {
		return
	}
	messages, err := s.store.Messages(room.ID)
	if err != nil {
		http.Error(w, "Could not read the room.", http.StatusInternalServerError)
		return
	}
	if in.Revision != nil && *in.Revision > len(messages) {
		http.Error(w, "Read the room again.", http.StatusConflict)
		return
	}
	for _, i := range in.Indices {
		if i < 0 || i >= len(messages) {
			http.Error(w, "Invalid message index.", http.StatusBadRequest)
			return
		}
	}
	m.Seen = time.Now()
	previous := *m
	m.ReadItems = maps.Clone(m.ReadItems)
	if m.ReadItems == nil {
		m.ReadItems = map[int]bool{}
	}
	for _, i := range in.Indices {
		if i >= m.Read {
			m.ReadItems[i] = true
		}
	}
	if in.Revision != nil && *in.Revision > m.Read {
		m.Read = *in.Revision
	}
	for m.ReadItems[m.Read] {
		delete(m.ReadItems, m.Read)
		m.Read++
	}
	for i := range m.ReadItems {
		if i < m.Read {
			delete(m.ReadItems, i)
		}
	}
	if m.Read == previous.Read && len(m.ReadItems) == len(previous.ReadItems) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.store.SaveMembers(s.members); err != nil {
		*m = previous
		http.Error(w, "Could not save the read receipt.", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

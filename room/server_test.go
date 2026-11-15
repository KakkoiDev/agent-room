package room

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type client struct {
	t    *testing.T
	base string
}

func (c client) do(method, path, token, body string, want int) string {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if res.StatusCode != want {
		c.t.Fatalf("%s %s: %d, want %d: %s", method, path, res.StatusCode, want, b.String())
	}
	return b.String()
}

func (c client) join(name, role string, want int) string {
	var out struct{ Token string }
	json.Unmarshal([]byte(c.do("POST", "/api/chat/rooms/general/participants", "", `{"username":"`+name+`","role":"`+role+`"}`, want)), &out)
	return out.Token
}

// invite returns the agent invite code a person's "Invite an agent" hands out.
func (c client) invite(person string) string {
	c.t.Helper()
	text := c.do("GET", "/api/chat/rooms/general/invite?origin=https://box.example", person, "", 200)
	_, code, ok := strings.Cut(text, "&invite=")
	if !ok {
		c.t.Fatalf("invite has no code:\n%s", text)
	}
	return code[:strings.IndexByte(code, '"')]
}

// agent joins an agent with an invite code.
func (c client) agent(name, code string, want int) string {
	var out struct{ Token string }
	json.Unmarshal([]byte(c.do("POST", "/api/chat/rooms/general/participants", "", `{"username":"`+name+`","role":"agent","invite":"`+code+`"}`, want)), &out)
	return out.Token
}

func (c client) open(token string) []Request {
	var out struct{ Assignments []Request }
	json.Unmarshal([]byte(c.do("GET", "/api/chat/rooms/general/wait?after=999&timeout=0&work=1", token, "", 200)), &out)
	return out.Assignments
}

func mount(t *testing.T, opts Options) (*httptest.Server, client) {
	s := New(NewMemoryStore(), opts)
	var h http.Handler = s.Handler()
	if opts.BasePath != "" {
		mux := http.NewServeMux()
		mux.Handle(opts.BasePath+"/", http.StripPrefix(opts.BasePath, h))
		h = mux
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, client{t, srv.URL + opts.BasePath}
}

func TestMentionsHandoffsAndStop(t *testing.T) {
	_, c := mount(t, Options{})
	const api = "/api/chat/rooms/general"
	host := c.join("Host", "person", 201)
	code := c.invite(host)
	pi, other := c.agent("Pi", code, 201), c.agent("Other", code, 201)
	c.agent("pi", code, 409)

	c.do("POST", api, pi, `{"text":"hello","revision":0}`, 409) // nothing assigned yet
	c.do("POST", api+"/messages", host, `{"text":"@Pi please check","client_id":"a"}`, 200)
	c.do("POST", api+"/messages", host, `{"text":"@Pi please check","client_id":"a"}`, 200) // retry is idempotent
	work := c.open(pi)
	if len(work) != 1 || work[0].Prompt != "@Pi please check" {
		t.Fatalf("mention: %+v", work)
	}
	c.do("POST", api, pi, `{"text":"Looking now","revision":1}`, 204) // open request allows posting
	c.do("POST", api+"/assignments", pi, `{"assignment_id":"`+work[0].ID+`","action":"reply","request_id":"r1","text":"Done. @Other please verify"}`, 204)
	handed := c.open(other)
	if len(handed) != 1 || len(c.open(pi)) != 0 {
		t.Fatalf("handoff: %+v", handed)
	}
	c.do("POST", api+"/assignments", other, `{"assignment_id":"`+handed[0].ID+`","action":"done"}`, 204)
	if len(c.open(other)) != 0 {
		t.Fatal("done did not close the request")
	}
	c.do("POST", api+"/messages", host, `{"text":"@Other again","client_id":"b"}`, 200)
	again := c.open(other)
	c.do("POST", api+"/assignments", host, `{"assignment_id":"`+again[0].ID+`","action":"cancel"}`, 204)
	c.do("POST", api+"/assignments", other, `{"assignment_id":"`+again[0].ID+`","action":"reply","request_id":"late","text":"late"}`, 409)

	var now struct{ Revision int }
	json.Unmarshal([]byte(c.do("GET", api, host, "", 200)), &now)
	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("GET", c.base+api+"/wait?timeout=5&after="+strconv.Itoa(now.Revision), nil)
		req.Header.Set("Authorization", "Bearer "+pi)
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			res.Body.Close()
			done <- res.StatusCode
		}
	}()
	time.Sleep(100 * time.Millisecond)
	c.do("POST", api+"/participants/stop", pi, `{"username":"Pi"}`, 401)
	c.do("POST", api+"/participants/stop", host, `{"username":"Pi"}`, 204)
	select {
	case code := <-done:
		if code != http.StatusGone {
			t.Fatalf("stopped wait: %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not wake the waiting agent")
	}
	if got := c.do("GET", api+"/participants", host, "", 200); !strings.Contains(got, `"connection":"stopped"`) {
		t.Fatal(got)
	}
	var back struct{ Reclaimed bool }
	json.Unmarshal([]byte(c.do("POST", api+"/participants", "", `{"username":"Pi","role":"agent","invite":"`+code+`"}`, 201)), &back)
	if !back.Reclaimed {
		t.Fatal("stopped agent could not take its name back")
	}
	c.do("GET", api, pi, "", 401)
}

func TestInviteUsesBasePath(t *testing.T) {
	_, c := mount(t, Options{BasePath: "/agents"})
	c.do("GET", "/api/chat/rooms/general/invite?origin=https://box.example", "", "", 401) // only a person in the room can invite
	host := c.join("Host", "person", 201)
	text := c.do("GET", "/api/chat/rooms/general/invite?origin=https://box.example", host, "", 200)
	for _, want := range []string{`"https://box.example/agents/chat?room=general&invite=`, "https://box.example/agents/api/chat/client", "listen --timeout 500", "STOPPED"} {
		if !strings.Contains(text, want) {
			t.Fatalf("invite missing %q:\n%s", want, text)
		}
	}
	c.do("GET", "/api/chat/rooms/general/invite?origin=javascript:alert(1)", host, "", 400)
	if page := c.do("GET", "/chat", "", "", 200); !strings.Contains(page, `const BASE = "/agents";`) {
		t.Fatal("page does not know its base path")
	}
}

func TestAuthorizeRefuses(t *testing.T) {
	_, c := mount(t, Options{Authorize: func(r *http.Request) error {
		if r.Header.Get("X-Allowed") == "" {
			return errors.New("not allowed")
		}
		return nil
	}})
	c.do("GET", "/api/chat/rooms", "", "", 403)
}

// TestHelperEndToEnd drives the real helper the way a pasted invite tells an agent to.
func TestHelperEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	srv, c := mount(t, Options{BasePath: "/agents", HouseAgent: "bot"})
	dir := t.TempDir()
	script := filepath.Join(dir, "agent-room.py")
	if err := os.WriteFile(script, []byte(c.do("GET", "/api/chat/client", "", "", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	host := c.join("Host", "person", 201)
	invite := srv.URL + "/agents/chat?room=general&invite=" + c.invite(host)
	link := invite
	helper := func(want int, args ...string) string {
		t.Helper()
		cmd := exec.Command("python3", append([]string{"-I", script, link, "bot"}, args...)...)
		cmd.Env = append(os.Environ(), "XDG_STATE_HOME="+dir)
		out, err := cmd.CombinedOutput()
		code := 0
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if code != want {
			t.Fatalf("helper %v: exit %d, want %d:\n%s", args, code, want, out)
		}
		return string(out)
	}
	link = srv.URL + "/agents/chat?room=general" // the room link alone
	if out := helper(1, "check"); !strings.Contains(out, "no invite code") {
		t.Fatal(out)
	}
	link = invite + "0"
	if out := helper(1, "check"); !strings.Contains(out, "not valid") {
		t.Fatal(out)
	}
	link = invite
	if out := helper(0, "check"); !strings.Contains(out, "OK: this machine can reach") {
		t.Fatal(out)
	}
	if out := helper(0, "join"); !strings.Contains(out, `Joined "General" as bot`) {
		t.Fatal(out)
	}
	if out := helper(0, "listen", "--timeout", "1"); !strings.Contains(out, "No request yet") {
		t.Fatal(out)
	}
	long := strings.Repeat("x", 6000) // longer prompts used to be cut at 4,000 characters
	c.do("POST", "/api/chat/rooms/general/messages", host, `{"text":"@bot what is 2+2? `+long+` END"}`, 200)
	out := helper(0, "listen", "--timeout", "5")
	if !strings.Contains(out, "what is 2+2? "+long+" END") || !strings.Contains(out, "From: Host") {
		t.Fatal(out)
	}
	id := strings.TrimSpace(strings.SplitN(strings.SplitN(out, "Request ID: ", 2)[1], "\n", 2)[0])
	if out := helper(0, "reply", id, "4"); !strings.Contains(out, "Reply sent") {
		t.Fatal(out)
	}
	if got := c.do("GET", "/api/chat/rooms/general", host, "", 200); !strings.Contains(got, `"text":"4"`) {
		t.Fatal("reply missing from the room")
	}
	// bot is the house agent: an untagged message reaches it as an ambient request.
	c.do("POST", "/api/chat/rooms/general/messages", host, `{"text":"thanks, that is all for today"}`, 200)
	out = helper(0, "listen", "--timeout", "5")
	if !strings.Contains(out, "Ambient: yes") || !strings.Contains(out, "done REQUEST_ID") {
		t.Fatal(out)
	}
	id = strings.TrimSpace(strings.SplitN(strings.SplitN(out, "Request ID: ", 2)[1], "\n", 2)[0])
	if out := helper(0, "done", id); !strings.Contains(out, "Request closed") {
		t.Fatal(out)
	}
	// Revoking the invite cuts the agent off; it exits like a stopped agent.
	c.do("POST", "/api/chat/rooms/general/invite/revoke", host, `{}`, 204)
	if out := helper(3, "listen", "--timeout", "5"); !strings.Contains(out, "revoked") {
		t.Fatal(out)
	}
	if out := helper(1, "join"); !strings.Contains(out, "not valid") {
		t.Fatal(out)
	}
	invite = srv.URL + "/agents/chat?room=general&invite=" + c.invite(host)
	link = invite
	if out := helper(0, "join"); !strings.Contains(out, "took it over") {
		t.Fatal(out)
	}
	c.do("POST", "/api/chat/rooms/general/participants/stop", host, `{"username":"bot"}`, 204)
	if out := helper(3, "listen", "--timeout", "5"); !strings.Contains(out, "STOPPED") {
		t.Fatal(out)
	}
}

// fetch sends a request with extra headers and returns the response with its body read.
func fetch(t *testing.T, method, url string, headers map[string]string, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func TestCORS(t *testing.T) {
	refuse := errors.New("not allowed")
	_, c := mount(t, Options{
		AllowedOrigins: []string{"https://mini-flow.kakkoi.dev/", "http://localhost:*", "not an origin"},
		Authorize: func(r *http.Request) error {
			if r.Header.Get("Authorization") == "" && r.Method == http.MethodOptions {
				return refuse // preflights must be answered before Authorize
			}
			return nil
		},
	})
	const app = "https://mini-flow.kakkoi.dev"
	preflight := func(origin string, pna bool) *http.Response {
		h := map[string]string{"Origin": origin, "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "authorization,content-type"}
		if pna {
			h["Access-Control-Request-Private-Network"] = "true"
		}
		res, _ := fetch(t, "OPTIONS", c.base+"/api/chat/rooms/general/messages", h, "")
		return res
	}
	for _, origin := range []string{app, "http://localhost:5173", "http://localhost"} {
		res := preflight(origin, false)
		h := res.Header
		if res.StatusCode != 204 || h.Get("Access-Control-Allow-Origin") != origin || h.Get("Vary") != "Origin" ||
			h.Get("Access-Control-Allow-Headers") != "Authorization, Content-Type" || h.Get("Access-Control-Allow-Methods") != "GET, POST" ||
			h.Get("Access-Control-Max-Age") != "600" || h.Get("Access-Control-Allow-Private-Network") != "" || h.Get("Access-Control-Allow-Credentials") != "" {
			t.Fatalf("preflight %s: %d %v", origin, res.StatusCode, h)
		}
	}
	if res := preflight(app, true); res.StatusCode != 204 || res.Header.Get("Access-Control-Allow-Private-Network") != "true" {
		t.Fatalf("private network preflight: %d %v", res.StatusCode, res.Header)
	}
	for _, origin := range []string{"https://evil.example", "http://localhost.evil.example:80", "http://localhost:80x", "https://localhost:3000", "null"} {
		if res := preflight(origin, true); res.StatusCode == 204 || res.Header.Get("Access-Control-Allow-Origin") != "" || res.Header.Get("Access-Control-Allow-Private-Network") != "" {
			t.Fatalf("preflight from %s: %d %v", origin, res.StatusCode, res.Header)
		}
	}

	host := c.join("Host", "person", 201)
	post := func(origin, site, text string) (*http.Response, string) {
		return fetch(t, "POST", c.base+"/api/chat/rooms/general/messages", map[string]string{
			"Origin": origin, "Sec-Fetch-Site": site, "Authorization": "Bearer " + host, "Content-Type": "application/json",
		}, `{"text":"`+text+`"}`)
	}
	if res, body := post(app, "cross-site", "from mini flow"); res.StatusCode != 200 || res.Header.Get("Access-Control-Allow-Origin") != app {
		t.Fatalf("allowed cross-origin post: %d %v %s", res.StatusCode, res.Header, body)
	}
	if res, body := post("http://localhost:5173", "same-site", "from dev"); res.StatusCode != 200 {
		t.Fatalf("port wildcard post: %d %s", res.StatusCode, body)
	}
	if res, _ := post("https://evil.example", "cross-site", "forged"); res.StatusCode != 403 || res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("disallowed cross-origin post: %d %v", res.StatusCode, res.Header)
	}
	if res, _ := post("http://localhost:5173.evil.example", "cross-site", "forged"); res.StatusCode != 403 {
		t.Fatalf("lookalike origin post: %d", res.StatusCode)
	}
	// Reading a room needs a session (or an agent invite), from any origin.
	viewer := c.join("Viewer", "person", 201)
	res, body := fetch(t, "GET", c.base+"/api/chat/rooms/general", map[string]string{"Origin": app, "Authorization": "Bearer " + viewer}, "")
	if res.StatusCode != 200 || res.Header.Get("Access-Control-Allow-Origin") != app || strings.Contains(body, "forged") || !strings.Contains(body, "from dev") {
		t.Fatalf("allowed get: %d %v %s", res.StatusCode, res.Header, body)
	}
	if res, _ := fetch(t, "GET", c.base+"/api/chat/rooms", map[string]string{"Origin": "https://evil.example"}, ""); res.StatusCode != 200 || res.Header.Get("Access-Control-Allow-Origin") != "" || res.Header.Get("Vary") != "" {
		t.Fatalf("disallowed get: %v", res.Header)
	}
}

func TestHostCheck(t *testing.T) {
	_, c := mount(t, Options{AllowedHosts: []string{"127.0.0.1", "localhost", "::1", "*.ts.net", "Box.Example."}})
	for host, want := range map[string]int{
		"127.0.0.1:1234": 200, "localhost": 200, "[::1]:80": 200, "box.tailnet.ts.net": 200, "box.example:8790": 200,
		"evil.example": 421, "ts.net": 421, "evil-ts.net": 421, "127.0.0.1.evil.example:80": 421,
	} {
		res, body := fetch(t, "GET", c.base+"/api/chat/rooms", map[string]string{"Host": host}, "")
		if res.StatusCode != want {
			t.Errorf("Host %s: %d, want %d: %s", host, res.StatusCode, want, body)
		}
	}
	// Refused before Authorize or preflight handling.
	_, c = mount(t, Options{AllowedHosts: []string{"localhost"}, AllowedOrigins: []string{"http://localhost:*"},
		Authorize: func(*http.Request) error { return errors.New("not allowed") }})
	if res, _ := fetch(t, "OPTIONS", c.base+"/api/chat/rooms", map[string]string{"Host": "rebound.example", "Origin": "http://localhost:3000"}, ""); res.StatusCode != 421 {
		t.Fatalf("rebound preflight: %d", res.StatusCode)
	}
	if res, _ := fetch(t, "GET", c.base+"/api/chat/rooms", map[string]string{"Host": "rebound.example", "Tailscale-User-Login": "you@example.com"}, ""); res.StatusCode != 421 {
		t.Fatalf("rebound get: %d", res.StatusCode)
	}
}

func TestPersonWait(t *testing.T) {
	_, c := mount(t, Options{})
	const api = "/api/chat/rooms/general"
	alice, bob := c.join("Alice", "person", 201), c.join("Bob", "person", 201)
	bot := c.agent("bot", c.invite(alice), 201)
	c.do("GET", api+"/wait?after=0&timeout=0", "", "", 401)

	type update struct {
		Revision    int
		NextAfter   int `json:"next_after"`
		Messages    []Message
		Assignments []Request
		CanPost     bool `json:"can_post"`
	}
	done := make(chan update, 1)
	go func() {
		var u update
		json.Unmarshal([]byte(c.do("GET", api+"/wait?after=0&timeout=5&work=1", bob, "", 200)), &u)
		done <- u
	}()
	time.Sleep(100 * time.Millisecond)
	if got := c.do("GET", api+"/participants", alice, "", 200); strings.Count(got, `"listening":true`) != 0 || !strings.Contains(got, `"username":"Bob","role":"person","seen"`) {
		t.Fatalf("a waiting person counts as listening: %s", got)
	}
	c.do("POST", api+"/messages", alice, `{"text":"@bot hi @Bob"}`, 200)
	select {
	case u := <-done:
		if u.Revision != 1 || u.NextAfter != 1 || len(u.Messages) != 1 || u.Messages[0].Text != "@bot hi @Bob" || u.Assignments == nil || len(u.Assignments) != 0 || !u.CanPost {
			t.Fatalf("person wait: %+v", u)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a new message did not wake the waiting person")
	}
	// With nothing new, work=1 does not return at once for a person.
	start := time.Now()
	c.do("GET", api+"/wait?after=1&timeout=1&work=1", bob, "", 200)
	if time.Since(start) < 900*time.Millisecond {
		t.Fatal("person wait returned early")
	}
	if len(c.open(bot)) != 1 {
		t.Fatal("agent lost its request")
	}
}

func TestJoinNameLengthCountsCharacters(t *testing.T) {
	_, c := mount(t, Options{})
	c.join(strings.Repeat("あ", 80), "person", 201)
	c.join(strings.Repeat("い", 81), "person", 400)
}

func TestAgentsNeedAValidInvite(t *testing.T) {
	store := NewMemoryStore()
	// An agent session from before invites existed: no inviter.
	store.SaveMembers(Members{"general": {sessionKey("old-token"): {Name: "Old", Role: "agent"}}})
	s := New(store, Options{})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	c := client{t, srv.URL}
	const api = "/api/chat/rooms/general"

	// The room link alone shows nothing and lets no agent in.
	c.do("GET", api, "", "", 401)
	c.do("GET", api+"/participants", "", "", 401)
	c.agent("Bot", "", 403)
	c.agent("Bot", "0123456789abcdef0123456789abcdef", 403)

	host, guest := c.join("Host", "person", 201), c.join("Guest", "person", 201)
	code := c.invite(host)
	if again := c.invite(host); again != code {
		t.Fatal("a person's invite changed without a revoke")
	}
	if c.invite(guest) == code {
		t.Fatal("two people share an invite")
	}
	if other, _ := s.InviteCode("elsewhere", "Host"); other != "" {
		t.Fatal("invite for a room the person is not in")
	}
	c.do("GET", api+"?invite=nope", "", "", 403)
	c.do("GET", api+"?invite="+code, "", "", 200) // check
	c.do("GET", api+"/participants?invite="+code, "", "", 200)

	bot := c.agent("Bot", code, 201)
	guests := c.agent("GuestBot", c.invite(guest), 201)
	if got := c.do("GET", api+"/participants", host, "", 200); !strings.Contains(got, `"invited_by":"Host"`) || strings.Contains(got, "invite_key") {
		t.Fatal(got)
	}
	c.do("POST", api+"/messages", host, `{"text":"@Bot @GuestBot @Old hello","client_id":"a"}`, 200)
	if len(c.open(bot)) != 1 || len(c.open(guests)) != 1 || len(c.open("old-token")) != 1 {
		t.Fatal("invited and legacy agents should all get requests")
	}

	// Host revokes: Host's agents are cut off at once (a waiting one too), others keep working.
	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("GET", c.base+api+"/wait?timeout=5&after=1", nil)
		req.Header.Set("Authorization", "Bearer "+bot)
		if res, err := http.DefaultClient.Do(req); err == nil {
			res.Body.Close()
			done <- res.StatusCode
		}
	}()
	time.Sleep(100 * time.Millisecond)
	c.do("POST", api+"/invite/revoke", bot, `{}`, 401) // agents cannot revoke
	c.do("POST", api+"/invite/revoke", host, `{}`, 204)
	select {
	case code := <-done:
		if code != http.StatusGone {
			t.Fatalf("revoked wait: %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revoking did not wake the waiting agent")
	}
	for _, path := range []string{api, api + "/participants", api + "/wait?after=0&timeout=0"} {
		if got := c.do("GET", path, bot, "", 410); !strings.Contains(got, "revoked") {
			t.Fatal(got)
		}
	}
	c.do("POST", api+"/read", bot, `{"revision":1}`, 410)
	if got := c.do("GET", api+"/participants", host, "", 200); !strings.Contains(got, `"connection":"revoked"`) {
		t.Fatal(got)
	}
	c.open(guests)
	c.open("old-token")
	c.agent("Bot2", code, 403) // the old code no longer works
	c.do("GET", api+"?invite="+code, "", "", 403)

	// A new invite works, and takes the revoked agent's name back at once.
	var back struct{ Reclaimed bool }
	json.Unmarshal([]byte(c.do("POST", api+"/participants", "", `{"username":"Bot","role":"agent","invite":"`+c.invite(host)+`"}`, 201)), &back)
	if !back.Reclaimed {
		t.Fatal("revoked agent's name was not reclaimable")
	}
	// A legacy agent cannot join again without an invite.
	c.agent("Old", "", 403)
}

func TestHouseAgentAnswersUntaggedMessages(t *testing.T) {
	_, c := mount(t, Options{HouseAgent: "mini"})
	const api = "/api/chat/rooms/general"
	host := c.join("Host", "person", 201)
	c.join("Mini", "person", 409) // the house agent's name is reserved
	var before struct {
		Requests []Request
	}
	c.do("POST", api+"/messages", host, `{"text":"anyone around?","client_id":"0"}`, 200)
	json.Unmarshal([]byte(c.do("GET", api, host, "", 200)), &before)
	if len(before.Requests) != 0 {
		t.Fatal("ambient request for a house agent that is not in the room")
	}

	code := c.invite(host)
	mini, pi := c.agent("mini", code, 201), c.agent("Pi", code, 201)
	post := func(id, text string) []Request {
		var out struct{ Assignments []Request }
		json.Unmarshal([]byte(c.do("POST", api+"/messages", host, `{"text":"`+text+`","client_id":"`+id+`"}`, 200)), &out)
		return out.Assignments
	}
	if got := post("a", "what is 2+2?"); len(got) != 1 || got[0].Agent != "mini" || !got[0].Ambient {
		t.Fatalf("untagged: %+v", got)
	}
	if got := post("b", "@Pi thoughts?"); len(got) != 1 || got[0].Agent != "Pi" || got[0].Ambient {
		t.Fatalf("tagged an agent: %+v", got)
	}
	if got := post("c", "@Host can you look?"); len(got) != 0 {
		t.Fatalf("tagged a person: %+v", got)
	}
	if got := post("d", "@everyone lunch?"); len(got) != 0 {
		t.Fatalf("tagged everyone: %+v", got)
	}
	if got := post("e", "mail me at a@b.example"); len(got) != 1 || !got[0].Ambient {
		t.Fatalf("an email address is not a tag: %+v", got)
	}
	if got := post("f", "@mini summarize"); len(got) != 1 || got[0].Ambient {
		t.Fatalf("@mini: %+v", got)
	}
	// Agents' own messages never go to the house agent.
	c.do("POST", api+"/assignments", pi, `{"assignment_id":"`+c.open(pi)[0].ID+`","action":"reply","request_id":"p","text":"four"}`, 204)

	work := c.open(mini)
	if len(work) != 3 || !work[0].Ambient || !work[1].Ambient || work[2].Ambient {
		t.Fatalf("mini's work: %+v", work)
	}
	// It stays silent on an ambient request by closing it with done.
	c.do("POST", api+"/assignments", mini, `{"assignment_id":"`+work[1].ID+`","action":"done"}`, 204)
	if len(c.open(mini)) != 2 {
		t.Fatal("done did not close the ambient request")
	}

	// A person turns it off for the room; the setting is in the transcript.
	c.do("POST", api+"/house-agent", mini, `{"enabled":false}`, 401)
	c.do("POST", api+"/house-agent", host, `{"enabled":false}`, 204)
	if got := post("g", "just thinking out loud"); len(got) != 0 {
		t.Fatalf("house agent off: %+v", got)
	}
	var state struct {
		HouseAgent struct {
			Name    string
			Enabled bool
		} `json:"house_agent"`
	}
	json.Unmarshal([]byte(c.do("GET", api, host, "", 200)), &state)
	if state.HouseAgent.Name != "mini" || state.HouseAgent.Enabled {
		t.Fatalf("state: %+v", state)
	}
	c.do("POST", api+"/house-agent", host, `{"enabled":true}`, 204)
	if got := post("h", "back on?"); len(got) != 1 || !got[0].Ambient {
		t.Fatalf("house agent on: %+v", got)
	}

	// A stopped house agent gets nothing.
	c.do("POST", api+"/participants/stop", host, `{"username":"mini"}`, 204)
	if got := post("i", "hello again"); len(got) != 0 {
		t.Fatalf("stopped house agent: %+v", got)
	}

	// Without -house-agent, the setting does not exist.
	_, plain := mount(t, Options{})
	plain.do("POST", api+"/house-agent", plain.join("Host", "person", 201), `{"enabled":false}`, 404)
}

func TestInvitesSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	start := func() *Server {
		store, err := NewFileStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		return New(store, Options{})
	}
	first := start()
	srv := httptest.NewServer(first.Handler())
	client{t, srv.URL}.join("Host", "person", 201)
	srv.Close()
	code, err := first.InviteCode("general", "Host")
	if err != nil || code == "" {
		t.Fatal(code, err)
	}
	if again, _ := start().InviteCode("general", "Host"); again != code {
		t.Fatal("invite changed after a restart")
	}
}

package room

import (
	"encoding/json"
	"errors"
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
	host, pi, other := c.join("Host", "person", 201), c.join("Pi", "agent", 201), c.join("Other", "agent", 201)
	c.join("pi", "agent", 409)

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
	json.Unmarshal([]byte(c.do("GET", api, "", "", 200)), &now)
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
	if got := c.do("GET", api+"/participants", "", "", 200); !strings.Contains(got, `"connection":"stopped"`) {
		t.Fatal(got)
	}
	var back struct{ Reclaimed bool }
	json.Unmarshal([]byte(c.do("POST", api+"/participants", "", `{"username":"Pi","role":"agent"}`, 201)), &back)
	if !back.Reclaimed {
		t.Fatal("stopped agent could not take its name back")
	}
	c.do("GET", api, pi, "", 401)
}

func TestInviteUsesBasePath(t *testing.T) {
	_, c := mount(t, Options{BasePath: "/agents"})
	text := c.do("GET", "/api/chat/rooms/general/invite?origin=https://box.example", "", "", 200)
	for _, want := range []string{`"https://box.example/agents/chat?room=general"`, "https://box.example/agents/api/chat/client", "listen --timeout 500", "STOPPED"} {
		if !strings.Contains(text, want) {
			t.Fatalf("invite missing %q:\n%s", want, text)
		}
	}
	c.do("GET", "/api/chat/rooms/general/invite?origin=javascript:alert(1)", "", "", 400)
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
	srv, c := mount(t, Options{BasePath: "/agents"})
	dir := t.TempDir()
	script := filepath.Join(dir, "agent-room.py")
	if err := os.WriteFile(script, []byte(c.do("GET", "/api/chat/client", "", "", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	invite := srv.URL + "/agents/chat?room=general"
	helper := func(want int, args ...string) string {
		t.Helper()
		cmd := exec.Command("python3", append([]string{"-I", script, invite, "bot"}, args...)...)
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
	if out := helper(0, "check"); !strings.Contains(out, "OK: this machine can reach") {
		t.Fatal(out)
	}
	if out := helper(0, "join"); !strings.Contains(out, `Joined "General" as bot`) {
		t.Fatal(out)
	}
	if out := helper(0, "listen", "--timeout", "1"); !strings.Contains(out, "No request yet") {
		t.Fatal(out)
	}
	host := c.join("Host", "person", 201)
	c.do("POST", "/api/chat/rooms/general/messages", host, `{"text":"@bot what is 2+2?"}`, 200)
	out := helper(0, "listen", "--timeout", "5")
	if !strings.Contains(out, "what is 2+2?") || !strings.Contains(out, "From: Host") {
		t.Fatal(out)
	}
	id := strings.TrimSpace(strings.SplitN(strings.SplitN(out, "Request ID: ", 2)[1], "\n", 2)[0])
	if out := helper(0, "reply", id, "4"); !strings.Contains(out, "Reply sent") {
		t.Fatal(out)
	}
	if got := c.do("GET", "/api/chat/rooms/general", "", "", 200); !strings.Contains(got, `"text":"4"`) {
		t.Fatal("reply missing from the room")
	}
	c.do("POST", "/api/chat/rooms/general/participants/stop", host, `{"username":"bot"}`, 204)
	if out := helper(3, "listen", "--timeout", "5"); !strings.Contains(out, "STOPPED") {
		t.Fatal(out)
	}
}

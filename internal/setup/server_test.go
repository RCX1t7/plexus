package setup

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/RCX1t7/plexus/internal/config"
	"github.com/RCX1t7/plexus/internal/secrets"
)

func newServer(t *testing.T) (*Server, http.Handler, string) {
	dir := t.TempDir()
	cfg, _ := config.Load(dir)
	cfg.Bots = []config.Bot{{Name: "claude", DisplayName: "Claude", Harness: "claude_code", Enabled: true, Workdir: dir}}
	s := &Server{ConfigDir: dir, Config: cfg, Secrets: &secrets.Memory{}}
	if _, _, err := s.Listen(0); err != nil {
		t.Fatal(err)
	}
	return s, s.Handler(), "/s/" + s.pathToken + "/"
}

func post(h http.Handler, path string, form url.Values, host, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Host = host
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestPageGuards(t *testing.T) {
	_, h, base := newServer(t)
	for _, c := range []struct {
		path, host string
		want       int
	}{{base, "localhost:1", 200}, {base, "evil.example:1", 403}, {"/s/wrong/", "localhost:1", 404}} {
		r := httptest.NewRequest(http.MethodGet, c.path, nil)
		r.Host = c.host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Fatalf("%s %s: %d", c.host, c.path, w.Code)
		}
	}
	if w := post(h, base+"team", url.Values{"owners": {"U0SIN0001"}}, "localhost:1", "http://evil.example"); w.Code != 403 {
		t.Fatalf("cross-site post: %d", w.Code)
	}
}

func TestTeamPartnerAndStop(t *testing.T) {
	s, h, base := newServer(t)
	saved := 0
	s.OnSaved = func() { saved++ }
	var stopped string
	s.Stop = func(root string) (int, error) { stopped = root; return 2, nil }
	if w := post(h, base+"team", url.Values{"owners": {"U0SIN0001, U0SIN0002"}}, "localhost:1", ""); w.Code != 303 {
		t.Fatal(w.Code)
	}
	if len(s.Config.Owners) != 2 || s.Config.Guard() { // unchecked box = guard off
		t.Fatalf("%+v guard=%v", s.Config.Owners, s.Config.Guard())
	}
	post(h, base+"team", url.Values{"owners": {"U0SIN0001"}, "stranger_guard": {"on"}}, "localhost:1", "")
	if !s.Config.Guard() {
		t.Fatal("guard not re-enabled")
	}
	w := post(h, base+"team", url.Values{"owners": {"not-an-id"}}, "localhost:1", "")
	if !strings.Contains(w.Header().Get("Location"), "not+saved") || len(s.Config.Owners) != 1 {
		t.Fatalf("bad owner saved: %s", w.Header().Get("Location"))
	}
	w = post(h, base+"bot", url.Values{"name": {"claude"}, "enabled": {"on"}, "bot_token": {"xapp-wrong"}}, "localhost:1", "")
	if !strings.Contains(w.Header().Get("Location"), "xoxb") {
		t.Fatal("wrong token prefix accepted")
	}
	post(h, base+"bot", url.Values{"name": {"claude"}, "enabled": {"on"}, "bot_token": {"xoxb-1-2-abc"}, "app_token": {"xapp-1-A-abc"}}, "localhost:1", "")
	if v, err := s.Secrets.Get(secrets.BotTokenKey("claude")); err != nil || v != "xoxb-1-2-abc" {
		t.Fatal("bot token not stored")
	}
	w = post(h, base+"stop", url.Values{"task_id": {"C1:1.0"}}, "localhost:1", "")
	if stopped != "C1:1.0" || !strings.Contains(w.Header().Get("Location"), "stopped") {
		t.Fatal("stop not forwarded")
	}
	if saved < 2 {
		t.Fatal("OnSaved not called")
	}
	r := httptest.NewRequest(http.MethodGet, base, nil)
	r.Host = "localhost:1"
	page := httptest.NewRecorder()
	h.ServeHTTP(page, r)
	body := page.Body.String()
	for _, want := range []string{"Partner Claude", "Stranger guard", "Stop a task", "U0SIN0001"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "xoxb-1-2-abc") {
		t.Fatal("token rendered into the page")
	}
}

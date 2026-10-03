package setup

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/RCX1t7/plexus/internal/adapters/dsh"
	"github.com/RCX1t7/plexus/internal/config"
	"github.com/RCX1t7/plexus/internal/harness"
	"github.com/RCX1t7/plexus/internal/secrets"
	"github.com/RCX1t7/plexus/internal/store"
)

//go:embed web/index.html
var webFS embed.FS

var page = template.Must(template.ParseFS(webFS, "web/index.html"))

// Server is the local setup page.
type Server struct {
	ConfigDir string
	Config    *config.Config
	Secrets   secrets.Store
	Store     *store.Store
	Detected  []harness.DetectionResult
	Log       *slog.Logger
	// SlackAPI is the Web API base URL (tests point it at a fake).
	SlackAPI string
	HTTP     *http.Client
	// OnSaved is called after tokens or settings change (the supervisor
	// uses it to start new bots without a restart).
	OnSaved func()
	// Stop stops a task tree in the running hub (nil: revoke in the store).
	Stop func(root string) (int, error)
	// DSHHome is where the bundled DSH bridge plugin is installed.
	DSHHome string

	pathToken string
	port      int
	mu        sync.Mutex
	oauth     map[string]oauthState // state -> pending install
}

type oauthState struct {
	bot, clientID, clientSecret string
	expires                     time.Time
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Listen binds 127.0.0.1:port (falling back to a random port) and returns
// the one-time URL of the page.
func (s *Server) Listen(port int) (net.Listener, string, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return nil, "", err
		}
		s.logger().Warn("setup port busy; OAuth install links will not work this run", "wanted", port)
	}
	s.port = ln.Addr().(*net.TCPAddr).Port
	s.pathToken = randHex(16)
	s.oauth = map[string]oauthState{}
	if s.SlackAPI == "" {
		s.SlackAPI = "https://slack.com/api/"
	}
	if s.HTTP == nil {
		s.HTTP = &http.Client{Timeout: 20 * time.Second}
	}
	return ln, fmt.Sprintf("http://localhost:%d/s/%s/", s.port, s.pathToken), nil
}

// Serve runs until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Handler exposes the routes (also used by tests).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	base := "/s/" + s.pathToken + "/"
	mux.HandleFunc("GET "+base, s.index)
	mux.HandleFunc("POST "+base+"team", s.saveTeam)
	mux.HandleFunc("POST "+base+"bot", s.saveBot)
	mux.HandleFunc("POST "+base+"dsh-plugin", s.installDSH)
	mux.HandleFunc("POST "+base+"oauth/start", s.oauthStart)
	mux.HandleFunc("POST "+base+"stop", s.stop)
	mux.HandleFunc("GET /oauth/callback", s.oauthCallback)
	return s.guard(mux)
}

// guard rejects requests that are not for localhost (DNS rebinding) and
// cross-site form posts.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.Host)
		if host != "localhost" && host != "127.0.0.1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost {
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'")
		next.ServeHTTP(w, r)
	})
}

type botView struct {
	config.Bot
	HasBot    bool
	HasApp    bool
	CreateURL string
	Detection *harness.DetectionResult
	CanOAuth  bool
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var bots []botView
	for _, b := range s.Config.Bots {
		v := botView{Bot: b, CanOAuth: s.port != 0}
		_, e1 := s.Secrets.Get(secrets.BotTokenKey(b.Name))
		_, e2 := s.Secrets.Get(secrets.AppTokenKey(b.Name))
		v.HasBot, v.HasApp = e1 == nil, e2 == nil
		v.CreateURL = CreateAppURL(Manifest(b.DisplayName, b.Harness, s.port))
		for i := range s.Detected {
			if s.Detected[i].Harness == b.Harness {
				v.Detection = &s.Detected[i]
			}
		}
		bots = append(bots, v)
	}
	var revoked []string
	if s.Store != nil {
		revoked, _ = s.Store.Revocations(20)
	}
	hasDSH := false
	for _, d := range s.Detected {
		hasDSH = hasDSH || (d.Harness == "dsh" && d.Installed)
	}
	_ = page.Execute(w, map[string]any{"Bots": bots, "Detected": s.Detected, "Base": "/s/" + s.pathToken + "/",
		"Revoked": revoked, "Msg": r.URL.Query().Get("msg"), "Owners": strings.Join(s.Config.Owners, ", "),
		"Guard": s.Config.Guard(), "DSH": hasDSH, "DSHBundled": dsh.Bundled()})
}

func (s *Server) back(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/s/"+s.pathToken+"/?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

func splitIDs(v string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		out = append(out, strings.TrimSpace(f))
	}
	return out
}

func (s *Server) saveBot(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	b := s.Config.Bot(r.PostFormValue("name"))
	if b == nil {
		s.mu.Unlock()
		http.Error(w, "unknown partner", http.StatusNotFound)
		return
	}
	old := *b
	if wd := strings.TrimSpace(r.PostFormValue("workdir")); wd != "" {
		b.Workdir = wd
	}
	b.Enabled = r.PostFormValue("enabled") == "on"
	if err := s.Config.Save(s.ConfigDir); err != nil {
		*b = old
		s.mu.Unlock()
		s.back(w, r, "not saved: "+err.Error())
		return
	}
	name := b.Name
	s.mu.Unlock()
	msg := "saved"
	if t := strings.TrimSpace(r.PostFormValue("bot_token")); t != "" {
		if !strings.HasPrefix(t, "xoxb-") {
			s.back(w, r, "the Bot User OAuth Token starts with xoxb-")
			return
		}
		if err := s.Secrets.Set(secrets.BotTokenKey(name), t); err != nil {
			s.back(w, r, "could not store the token securely: "+err.Error())
			return
		}
	}
	if t := strings.TrimSpace(r.PostFormValue("app_token")); t != "" {
		if !strings.HasPrefix(t, "xapp-") {
			s.back(w, r, "the App-Level Token starts with xapp-")
			return
		}
		if err := s.Secrets.Set(secrets.AppTokenKey(name), t); err != nil {
			s.back(w, r, "could not store the token securely: "+err.Error())
			return
		}
	}
	if s.OnSaved != nil {
		s.OnSaved()
	}
	s.back(w, r, msg)
}

// oauthStart begins an OAuth v2 install with the app's client id/secret
// (kept in memory only, for ten minutes).
func (s *Server) oauthStart(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name, id, secret := r.PostFormValue("name"), strings.TrimSpace(r.PostFormValue("client_id")), strings.TrimSpace(r.PostFormValue("client_secret"))
	if s.Config.Bot(name) == nil || id == "" || secret == "" {
		s.back(w, r, "client ID and client secret are both needed")
		return
	}
	state := randHex(16)
	s.mu.Lock()
	s.oauth[state] = oauthState{bot: name, clientID: id, clientSecret: secret, expires: time.Now().Add(10 * time.Minute)}
	s.mu.Unlock()
	http.Redirect(w, r, AuthorizeURL(id, state, s.port), http.StatusSeeOther)
}

func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	s.mu.Lock()
	st, ok := s.oauth[state]
	delete(s.oauth, state) // one-time
	s.mu.Unlock()
	if !ok || time.Now().After(st.expires) || code == "" {
		http.Error(w, "unknown or expired install request; start again from the setup page", http.StatusBadRequest)
		return
	}
	form := url.Values{"code": {code}, "redirect_uri": {RedirectURL(s.port)}}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, s.SlackAPI+"oauth.v2.access", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(st.clientID, st.clientSecret)
	resp, err := s.HTTP.Do(req)
	if err != nil {
		s.back(w, r, "Slack did not answer the token exchange; try again")
		return
	}
	defer resp.Body.Close()
	var out struct {
		OK          bool   `json:"ok"`
		Error       string `json:"error"`
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		AppID       string `json:"app_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || !out.OK || !strings.HasPrefix(out.AccessToken, "xoxb-") {
		s.back(w, r, "install failed: "+out.Error)
		return
	}
	if err := s.Secrets.Set(secrets.BotTokenKey(st.bot), out.AccessToken); err != nil {
		s.back(w, r, "could not store the token securely")
		return
	}
	s.mu.Lock()
	if b := s.Config.Bot(st.bot); b != nil && out.AppID != "" {
		b.AppID = out.AppID
		_ = s.Config.Save(s.ConfigDir)
	}
	s.mu.Unlock()
	if s.OnSaved != nil {
		s.OnSaved()
	}
	s.back(w, r, "bot token received for "+st.bot+". One step left: the App-Level Token (xapp-).")
}

// saveTeam stores Sin's Slack user IDs and the stranger guard switch.
func (s *Server) saveTeam(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	oldOwners, oldGuard := s.Config.Owners, s.Config.StrangerGuard
	s.Config.Owners = splitIDs(r.PostFormValue("owners"))
	s.Config.SetGuard(r.PostFormValue("stranger_guard") == "on")
	err := s.Config.Save(s.ConfigDir)
	if err != nil {
		s.Config.Owners, s.Config.StrangerGuard = oldOwners, oldGuard
	}
	s.mu.Unlock()
	if err != nil {
		s.back(w, r, "not saved: "+err.Error())
		return
	}
	if s.OnSaved != nil {
		s.OnSaved()
	}
	s.back(w, r, "saved")
}

// stop stops a task tree (all partners working on it).
func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.PostFormValue("task_id"))
	if id == "" {
		s.back(w, r, "enter a task id")
		return
	}
	if s.Stop != nil {
		n, err := s.Stop(id)
		if err != nil {
			s.back(w, r, "stop failed: "+err.Error())
			return
		}
		s.back(w, r, fmt.Sprintf("stopped %s (%d running sessions ended)", id, n))
		return
	}
	if s.Store == nil || s.Store.Revoke(id, "setup-page") != nil {
		s.back(w, r, "stop failed")
		return
	}
	s.back(w, r, "stopped "+id)
}

// installDSH installs the DSH bridge plugin bundled in this build.
func (s *Server) installDSH(w http.ResponseWriter, r *http.Request) {
	res, err := dsh.InstallAt(s.DSHHome)
	if err != nil {
		s.back(w, r, "plugin install failed: "+err.Error())
		return
	}
	s.back(w, r, "DSH bridge plugin installed in "+res.ProfileDir)
}

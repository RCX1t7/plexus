// Package supervisor is "plexus run": it loads the config, detects
// harnesses, and keeps one isolated, auto-restarting goroutine per bot.
package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/RCX1t7/plexus/internal/adapters/dsh"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"time"

	"github.com/RCX1t7/plexus/internal/adapters/acp"
	"github.com/RCX1t7/plexus/internal/config"
	"github.com/RCX1t7/plexus/internal/harness"
	"github.com/RCX1t7/plexus/internal/platform"
	"github.com/RCX1t7/plexus/internal/policy"
	"github.com/RCX1t7/plexus/internal/redact"
	"github.com/RCX1t7/plexus/internal/secrets"
	"github.com/RCX1t7/plexus/internal/setup"
	"github.com/RCX1t7/plexus/internal/slackbot"
	"github.com/RCX1t7/plexus/internal/store"
)

// Hub owns everything shared between bots.
type Hub struct {
	ConfigDir, DataDir string
	Config             *config.Config
	Store              *store.Store
	Secrets            secrets.Store
	Peers              *slackbot.Peers
	Stops              *slackbot.Stops
	Detected           []harness.DetectionResult
	Log                *slog.Logger
	LogWriter          *redact.Writer

	mu      sync.Mutex
	running map[string]*botRun
	control *control
	online  map[string]bool
	first   sync.WaitGroup // first connection attempt of the initial bots
}

type botRun struct {
	bot    config.Bot
	team   team
	cancel context.CancelFunc
}

// team is the hub-wide part of the config every partner depends on; a
// change restarts the partners.
type team struct {
	Owners string
	Guard  bool
	API    string
	Danger string
}

func teamOf(cfg *config.Config) team {
	d, _ := json.Marshal(cfg.DangerousActions)
	return team{Owners: fmt.Sprint(cfg.Owners), Guard: cfg.Guard(), API: cfg.SlackAPIURL, Danger: string(d)}
}

// Open prepares the hub: config (with harness auto-detection), store,
// secrets.
func Open(ctx context.Context, configDir, dataDir string, lw *redact.Writer) (*Hub, error) {
	log := slog.New(slog.NewTextHandler(lw, nil))
	for _, d := range []string{configDir, dataDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	cfg, err := config.Load(configDir)
	if err != nil {
		return nil, err
	}
	RegisterConfigACP(cfg, log)
	found := harness.DetectAll(ctx, harness.OSEnv())
	home, _ := os.UserHomeDir()
	if added := cfg.AutoAdd(found, home); len(added) > 0 {
		log.Info("added partners for detected harnesses", "partners", added)
		if err := cfg.Save(configDir); err != nil {
			return nil, err
		}
	}
	sec, err := secrets.Open(configDir)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(dataDir, "plexus.db"))
	if err != nil {
		return nil, err
	}
	for _, k := range harness.AuthEnv { // never logged, never touched
		if v := os.Getenv(k); v != "" {
			lw.AddSecret(v)
		}
	}
	return &Hub{ConfigDir: configDir, DataDir: dataDir, Config: cfg, Store: st, Secrets: sec,
		Peers: &slackbot.Peers{}, Stops: &slackbot.Stops{},
		Detected: found, Log: log, LogWriter: lw, running: map[string]*botRun{}, online: map[string]bool{}}, nil
}

// RegisterConfigACP adds the config-only ACP harnesses (long-tail agents).
func RegisterConfigACP(cfg *config.Config, log *slog.Logger) {
	for _, c := range cfg.ACP {
		if _, dup := harness.Get(c.Name); dup || c.Name == "" {
			log.Warn("skipping ACP entry with an empty or duplicate name", "name", c.Name)
			continue
		}
		harness.Register(acp.New(c))
	}
}

// Run starts every ready bot and, if some bot still lacks tokens, the setup
// page. It blocks until ctx ends.
func (h *Hub) Run(ctx context.Context, openBrowser bool) error {
	defer h.Store.Close()
	go h.prune(ctx)
	if err := h.serveControl(ctx); err != nil {
		h.Log.Warn("local control endpoint unavailable; `plexus stop` cannot reach this hub", "err", err.Error())
	}
	h.StartBots(ctx, h.Config)
	go func() {
		// The ready line tells scripts and tests the hub is up: every bot
		// with tokens has finished its first connection attempt.
		done := make(chan struct{})
		go func() { h.first.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(60 * time.Second):
		case <-ctx.Done():
			return
		}
		fmt.Fprintf(os.Stderr, "plexus ready bots=%d\n", h.Online())
	}()
	if h.missingTokens() || openBrowser {
		if err := h.Setup(ctx, true); err != nil {
			h.Log.Error("setup page failed", "err", err.Error())
		}
	}
	<-ctx.Done()
	return nil
}

// Setup serves the local setup page in the background.
func (h *Hub) Setup(ctx context.Context, open bool) error {
	srv := &setup.Server{ConfigDir: h.ConfigDir, Config: h.Config, Secrets: h.Secrets, Store: h.Store,
		Detected: h.Detected, Log: h.Log, OnSaved: func() { h.reload(ctx) },
		Stop: func(root string) (int, error) { return h.StopTask(ctx, root, "setup-page") }, DSHHome: dshHome()}
	ln, url, err := srv.Listen(h.Config.SetupPort)
	if err != nil {
		return err
	}
	// The one-time link goes to the console only (it is the page's password).
	fmt.Fprintln(os.Stderr, "Plexus setup:", url)
	if open {
		_ = platform.OpenBrowser(url)
	}
	go func() {
		if err := srv.Serve(ctx, ln); err != nil {
			h.Log.Error("setup server stopped", "err", err.Error())
		}
	}()
	return nil
}

func (h *Hub) missingTokens() bool {
	for _, b := range h.Config.Bots {
		if b.Enabled && !h.hasTokens(b.Name) {
			return true
		}
	}
	return len(h.Config.Bots) == 0
}

func (h *Hub) hasTokens(bot string) bool {
	_, e1 := h.Secrets.Get(secrets.BotTokenKey(bot))
	_, e2 := h.Secrets.Get(secrets.AppTokenKey(bot))
	return e1 == nil && e2 == nil
}

// reload re-reads the saved config (a fresh copy, never shared with the
// setup page) and applies it.
func (h *Hub) reload(ctx context.Context) {
	cfg, err := config.Load(h.ConfigDir)
	if err != nil {
		h.Log.Error("config reload failed", "err", err.Error())
		return
	}
	h.StartBots(ctx, cfg)
}

// StartBots makes the running bots match cfg: it starts enabled bots that
// have tokens, restarts bots whose settings changed (Owners, levels ...)
// and stops disabled ones. Safe to call repeatedly.
func (h *Hub) StartBots(ctx context.Context, cfg *config.Config) {
	h.mu.Lock()
	defer h.mu.Unlock()
	want := map[string]bool{}
	tm := teamOf(cfg)
	for _, b := range cfg.Bots {
		if !b.Enabled || !h.hasTokens(b.Name) {
			continue
		}
		hn, ok := harness.Get(b.Harness)
		if !ok {
			h.Log.Error("partner uses an unknown harness", "partner", b.Name, "harness", b.Harness)
			continue
		}
		if err := StartCheck(b, hn.Capabilities(), cfg.Guard()); err != nil {
			h.Log.Error("partner not started", "partner", b.Name, "err", err.Error())
			continue
		}
		want[b.Name] = true
		if r := h.running[b.Name]; r != nil {
			if reflect.DeepEqual(r.bot, b) && r.team == tm {
				continue
			}
			r.cancel()
		}
		bctx, cancel := context.WithCancel(ctx)
		h.running[b.Name] = &botRun{bot: b, team: tm, cancel: cancel}
		h.first.Add(1)
		var once sync.Once
		go h.supervise(bctx, cfg, b, hn, func() { once.Do(h.first.Done) })
	}
	for name, r := range h.running {
		if !want[name] {
			r.cancel()
			delete(h.running, name)
		}
	}
}

// supervise restarts one bot with exponential backoff (1s .. 5min). A
// failure or panic in one bot never affects the others.
func (h *Hub) supervise(ctx context.Context, cfg *config.Config, b config.Bot, hn harness.Harness, attempted func()) {
	defer attempted()
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := h.runOnce(ctx, cfg, b, hn, attempted)
		h.setOnline(b.Name, false)
		attempted()
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > 2*time.Minute {
			backoff = time.Second // it was healthy for a while
		}
		h.Log.Error("partner stopped; restarting", "partner", b.Name, "err", fmt.Sprint(err), "in", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}

// Online counts connected bots.
func (h *Hub) Online() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, up := range h.online {
		if up {
			n++
		}
	}
	return n
}

func (h *Hub) setOnline(bot string, up bool) {
	h.mu.Lock()
	h.online[bot] = up
	h.mu.Unlock()
}

func (h *Hub) runOnce(ctx context.Context, cfg *config.Config, b config.Bot, hn harness.Harness, connected func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	bt, err := h.Secrets.Get(secrets.BotTokenKey(b.Name))
	if err != nil {
		return err
	}
	at, err := h.Secrets.Get(secrets.AppTokenKey(b.Name))
	if err != nil {
		return err
	}
	h.LogWriter.AddSecret(bt)
	h.LogWriter.AddSecret(at)
	api, self, err := slackbot.Connect(ctx, bt, at, cfg.SlackAPIURL)
	if err != nil {
		return err
	}
	h.Peers.Add(self, b.Name)
	if n, _ := h.Store.RecoverStartup(b.Name); n > 0 {
		h.Log.Warn("messages whose send was interrupted are marked uncertain; checking Slack history before any resend", "bot", b.Name, "count", n)
	}
	if err := os.MkdirAll(b.Workdir, 0o755); err != nil {
		return err
	}
	ob := &slackbot.Outbox{Bot: b.Name, Store: h.Store, Poster: slackbot.SlackPoster{API: api}, Secrets: []string{bt, at}}
	_, _, _ = ob.Reconcile(ctx)
	_ = ob.Flush(ctx)
	go func() { // retry rate-limited rows, re-check uncertain ones
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_, _, _ = ob.Reconcile(ctx)
				_ = ob.Flush(ctx)
			}
		}
	}()
	w := &slackbot.Worker{Bot: b, Harness: hn, Store: h.Store, Outbox: ob, Policy: h.Policy(cfg, b),
		Owners: cfg.Owners, Peers: h.Peers, Stops: h.Stops, SelfID: self, Danger: cfg.DangerousActions, LockDir: filepath.Join(h.DataDir, "locks"),
		Log: h.Log.With("partner", b.Name), OriginWait: 2 * time.Second}
	h.Stops.Register(w)
	var recovered sync.Once
	return slackbot.Run(ctx, api, w, func() {
		h.Log.Info("partner online", "partner", b.Name, "harness", hn.Name(), "slack_user", self)
		h.setOnline(b.Name, true)
		connected()
		recovered.Do(func() { w.Recover(ctx) })
	})
}

// Policy builds the per-partner tool policy.
func (h *Hub) Policy(cfg *config.Config, b config.Bot) policy.Policy {
	home, _ := os.UserHomeDir()
	return policy.Policy{GOOS: runtime.GOOS, Home: home, Workdir: b.Workdir,
		StrangerGuard: cfg.Guard(), Revoker: h.Store}
}

// StopTask stops a task tree from outside Slack (CLI, setup page).
func (h *Hub) StopTask(ctx context.Context, root, by string) (int, error) {
	return h.Stops.Stop(ctx, h.Store, root, by)
}

func (h *Hub) prune(ctx context.Context) {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = h.Store.Prune(30 * 24 * time.Hour)
		}
	}
}

func dshHome() string { return dsh.DSHHome(harness.OSEnv()) }

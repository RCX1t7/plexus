// Package config is the non-secret configuration (config.json). Tokens
// never go here; they live in the secrets store.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/RCX1t7/plexus/internal/adapters/acp"
	"github.com/RCX1t7/plexus/internal/harness"
)

// Bot is one partner: one Slack App backed by one harness.
type Bot struct {
	Name        string   `json:"name"`         // unique id, also the secret-store key
	DisplayName string   `json:"display_name"` // Slack display name
	Harness     string   `json:"harness"`      // registry name, e.g. "claude_code"
	Enabled     bool     `json:"enabled"`
	Workdir     string   `json:"workdir"`
	Persona     string   `json:"persona,omitempty"`
	ExtraDeny   []string `json:"extra_deny,omitempty"` // paths strangers may never have read
	Exe         string   `json:"exe,omitempty"`        // executable override
	Args        []string `json:"args,omitempty"`       // extra harness args (e.g. a future --bare opt-out)
	AppID       string   `json:"app_id,omitempty"`
}

// Config is the whole file.
type Config struct {
	// Owners are Sin's Slack user IDs. Sin and all partners trust each other.
	Owners []string `json:"owners"`
	// StrangerGuard (default on): people who are neither an owner nor a
	// partner get conversation and help, but cannot make a partner run
	// commands or write files on this machine.
	StrangerGuard *bool        `json:"stranger_guard,omitempty"`
	Bots          []Bot        `json:"bots"`
	ACP           []acp.Config `json:"acp_harnesses,omitempty"` // config-only extra harnesses
	SetupPort     int          `json:"setup_port"`
	// SlackAPIURL overrides the Slack Web API base (default
	// https://slack.com/api/); Socket Mode's apps.connections.open uses it
	// too. For tests against a fake Slack.
	SlackAPIURL string `json:"slack_api_url,omitempty"`
}

// Guard reports whether the stranger guard is on.
func (c *Config) Guard() bool { return c.StrangerGuard == nil || *c.StrangerGuard }

// SetGuard switches the stranger guard.
func (c *Config) SetGuard(on bool) { c.StrangerGuard = &on }

var (
	slackUser = regexp.MustCompile(`^[UW][A-Z0-9]{2,}$`)
	botName   = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
)

// Defaults fills unset fields.
func (c *Config) Defaults() {
	if c.SetupPort == 0 {
		c.SetupPort = 47913
	}
	for i := range c.Bots {
		if c.Bots[i].DisplayName == "" {
			c.Bots[i].DisplayName = "Plexus " + c.Bots[i].Name
		}
	}
}

// Validate checks the config.
func (c *Config) Validate() error {
	seen := map[string]bool{}
	for _, b := range c.Bots {
		if !botName.MatchString(b.Name) {
			return fmt.Errorf("partner name %q: use a-z, 0-9, _ or -", b.Name)
		}
		if seen[b.Name] {
			return fmt.Errorf("duplicate partner %q", b.Name)
		}
		seen[b.Name] = true
	}
	for _, o := range c.Owners {
		if !slackUser.MatchString(o) {
			return fmt.Errorf("%q is not a Slack user ID (like U0123ABCD)", o)
		}
	}
	return nil
}

// Path is the config file location in dir.
func Path(dir string) string { return filepath.Join(dir, "config.json") }

// Load reads dir/config.json; a missing file gives an empty config.
func Load(dir string) (*Config, error) {
	c := &Config{}
	b, err := os.ReadFile(Path(dir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("config.json: %w", err)
		}
	}
	c.Defaults()
	return c, c.Validate()
}

// Save writes atomically.
func (c *Config) Save(dir string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := Path(dir) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, Path(dir))
}

// Bot returns the bot named name.
func (c *Config) Bot(name string) *Bot {
	for i := range c.Bots {
		if c.Bots[i].Name == name {
			return &c.Bots[i]
		}
	}
	return nil
}

// AutoAdd adds one partner per detected, installed harness that has none
// yet (zero-touch onboarding). It returns the names it added.
func (c *Config) AutoAdd(found []harness.DetectionResult, home string) []string {
	var added []string
	for _, d := range found {
		if !d.Installed || d.LoggedIn == harness.LoginNo {
			continue
		}
		if strings.HasSuffix(d.Harness, "_acp") {
			continue // fallbacks are never auto-added
		}
		exists := false
		for _, b := range c.Bots {
			if b.Harness == d.Harness {
				exists = true
			}
		}
		if exists {
			continue
		}
		c.Bots = append(c.Bots, Bot{Name: d.Harness, Harness: d.Harness, Enabled: true,
			DisplayName: "Plexus " + pretty(d.Harness), Workdir: filepath.Join(home, "PlexusWork", d.Harness)})
		added = append(added, d.Harness)
	}
	c.Defaults()
	return added
}

func pretty(h string) string {
	switch h {
	case "claude_code":
		return "Claude"
	case "codex":
		return "Codex"
	case "dsh":
		return "DSH"
	case "gemini_cli":
		return "Gemini"
	}
	return h
}

// Package setup is the local onboarding page: detected harnesses, one-click
// "create Slack app from manifest" links, token collection into the OS
// secret store, Owner / member-level settings and task revocation.
package setup

import (
	"encoding/json"
	"fmt"
	"net/url"
)

// BotScopes are the bot token scopes every Plexus app requests.
var BotScopes = []string{"app_mentions:read", "channels:history", "groups:history", "im:history",
	"mpim:history", "chat:write", "im:write", "users:read"}

// Manifest builds the Slack app manifest for one bot. Socket Mode is on, so
// no public URL is needed. The redirect URL lets the optional OAuth install
// deliver the bot token straight back to this machine.
func Manifest(displayName, harness string, port int) map[string]any {
	return map[string]any{
		"display_information": map[string]any{
			"name":             displayName,
			"description":      fmt.Sprintf("Plexus bridge to %s on my computer", harness),
			"background_color": "#1d2b3a",
		},
		"features": map[string]any{
			"bot_user": map[string]any{"display_name": displayName, "always_online": true},
			"app_home": map[string]any{"home_tab_enabled": false, "messages_tab_enabled": true,
				"messages_tab_read_only_enabled": false},
		},
		"oauth_config": map[string]any{
			"redirect_urls": []string{RedirectURL(port)},
			"scopes":        map[string]any{"bot": BotScopes},
		},
		"settings": map[string]any{
			"event_subscriptions":    map[string]any{"bot_events": []string{"message.channels", "message.groups", "message.im", "message.mpim"}},
			"interactivity":          map[string]any{"is_enabled": false},
			"org_deploy_enabled":     false,
			"socket_mode_enabled":    true,
			"token_rotation_enabled": false,
		},
	}
}

// RedirectURL is the fixed OAuth callback (Slack matches host and port
// exactly, so the setup page uses a fixed port).
func RedirectURL(port int) string { return fmt.Sprintf("http://localhost:%d/oauth/callback", port) }

// CreateAppURL is the one-click "Create New App" link with the manifest
// prefilled.
func CreateAppURL(manifest map[string]any) string {
	b, _ := json.Marshal(manifest)
	return "https://api.slack.com/apps?new_app=1&manifest_json=" + url.QueryEscape(string(b))
}

// AuthorizeURL is the OAuth v2 install link for an app.
func AuthorizeURL(clientID, state string, port int) string {
	q := url.Values{"client_id": {clientID}, "redirect_uri": {RedirectURL(port)}, "state": {state}}
	q.Set("scope", joinComma(BotScopes))
	return "https://slack.com/oauth/v2/authorize?" + q.Encode()
}

func joinComma(v []string) string {
	out := ""
	for i, s := range v {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

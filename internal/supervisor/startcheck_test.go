package supervisor

import (
	"strings"
	"testing"

	_ "github.com/RCX1t7/plexus/internal/adapters/claude"
	_ "github.com/RCX1t7/plexus/internal/adapters/codex"
	"github.com/RCX1t7/plexus/internal/config"
	"github.com/RCX1t7/plexus/internal/harness"
)

func TestStartCheck(t *testing.T) {
	n, e, u := harness.Native, harness.Emulated, harness.Unsupported
	b := config.Bot{Name: "p", Harness: "h"}
	ung := b
	ung.UngatedOK = true
	cases := []struct {
		name  string
		b     config.Bot
		caps  harness.Capabilities
		guard bool
		err   string // "" = may start
	}{
		{"native lock", b, harness.Capabilities{PermissionCallback: n, GuestLock: n}, true, ""},
		{"ignores strangers (Codex, ACP)", b, harness.Capabilities{PermissionCallback: n, GuestLock: u}, true, ""},
		{"accepts strangers without a native lock", b, harness.Capabilities{PermissionCallback: n, GuestLock: e}, true, "without a native lock"},
		{"ungated_ok does not waive the guest lock", ung, harness.Capabilities{PermissionCallback: u, GuestLock: e}, true, "without a native lock"},
		{"guard off: no strangers", b, harness.Capabilities{PermissionCallback: n, GuestLock: e}, false, ""},
		{"no blocking callback", b, harness.Capabilities{PermissionCallback: u, GuestLock: n}, true, "ungated_ok"},
		{"no blocking callback, ungated_ok", ung, harness.Capabilities{PermissionCallback: u, GuestLock: n}, true, ""},
	}
	for _, c := range cases {
		err := StartCheck(c.b, c.caps, c.guard)
		if (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// every built-in harness may start with the guard on
	for _, name := range []string{"claude_code", "codex", "dsh", "gemini_cli", "dsh_acp"} {
		h, ok := harness.Get(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if err := StartCheck(config.Bot{Name: name, Harness: name}, h.Capabilities(), true); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

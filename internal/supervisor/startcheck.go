package supervisor

import (
	"fmt"

	"github.com/RCX1t7/plexus/internal/config"
	"github.com/RCX1t7/plexus/internal/harness"
)

// StartCheck decides whether partner b may start on a harness with caps.
//
//   - Dangerous-action gate: the harness must offer a blocking permission
//     callback (PermissionCallback Native). bots[].ungated_ok waives this
//     one check, at Sin's risk.
//   - Stranger guard (when on): a harness either locks strangers' turns
//     natively (GuestLock Native) or ignores strangers outright
//     (GuestLock Unsupported: Codex and ACP today, which reply with one
//     refusal and run no turn). A harness that would accept strangers
//     without a native lock (GuestLock Emulated) must not start.
//     ungated_ok never waives this.
func StartCheck(b config.Bot, caps harness.Capabilities, guard bool) error {
	if guard && caps.GuestLock == harness.Emulated {
		return fmt.Errorf("partner %s: harness %s would take strangers' messages without a native lock; "+
			"it cannot start while the stranger guard is on (ungated_ok does not change this)", b.Name, b.Harness)
	}
	if caps.PermissionCallback != harness.Native && !b.UngatedOK {
		return fmt.Errorf("partner %s: harness %s has no blocking permission callback, so dangerous actions cannot wait for Sin's approval; "+
			"set \"ungated_ok\": true on this partner in config.json to run it anyway at your own risk", b.Name, b.Harness)
	}
	return nil
}

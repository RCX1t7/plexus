// Package all links every built-in adapter into the binary. Adding a
// harness = one new package under internal/adapters plus one import here.
package all

import (
	_ "github.com/RCX1t7/plexus/internal/adapters/acp"
	_ "github.com/RCX1t7/plexus/internal/adapters/claude"
	_ "github.com/RCX1t7/plexus/internal/adapters/codex"
	_ "github.com/RCX1t7/plexus/internal/adapters/dsh"
)

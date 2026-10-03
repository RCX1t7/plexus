// Package harness defines the one interface every coding-agent harness
// adapter implements, plus the registry adapters add themselves to.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// LoginState is tri-state: detection never sends a model request, so it can
// only report what local files and environment variables suggest.
type LoginState string

const (
	LoginYes     LoginState = "yes"
	LoginNo      LoginState = "no"
	LoginUnknown LoginState = "unknown"
)

// Support says how an adapter provides a feature.
type Support string

const (
	Native      Support = "native"      // the harness protocol has it
	Emulated    Support = "emulated"    // Plexus fakes it on top (e.g. persona prepended to the first prompt)
	Unsupported Support = "unsupported" // not available
)

// Capabilities is a per-feature support matrix.
type Capabilities struct {
	PermissionCallback Support `json:"permission_callback"` // every tool call can be gated by Plexus
	AskUser            Support `json:"ask_user"`            // structured questions to the user
	BackgroundTasks    Support `json:"background_tasks"`    // jobs that outlive a turn
	Subagents          Support `json:"subagents"`           // lineage via ParentID
	SlashCommands      Support `json:"slash_commands"`
	Effort             Support `json:"effort"`    // reasoning effort control
	Resume             Support `json:"resume"`    // resume by native session id
	Interrupt          Support `json:"interrupt"` // stop a running turn
	StopHook           Support `json:"stop_hook"` // native hook when the agent wants to stop
	SystemPrompt       Support `json:"system_prompt"`
	Control            Support `json:"control"`       // raw Control(name, payload) passthrough
	PerTaskStop        Support `json:"per_task_stop"` // stop one background task / subagent
	HostTools          Support `json:"host_tools"`    // Plexus tools (plexus_post ...) mounted natively
	// GuestLock: every tool call of a stranger's turn reaches the Plexus
	// policy (no "safe" command runs without a callback). Harnesses without
	// it do not take strangers' messages while the stranger guard is on.
	GuestLock Support `json:"guest_lock"`
}

// ErrUnsupported is returned by Control for unknown names.
var ErrUnsupported = errors.New("harness: unsupported")

// DetectionResult is what Detect found on this machine.
type DetectionResult struct {
	Harness   string       `json:"harness"`
	Installed bool         `json:"installed"`
	Path      string       `json:"path,omitempty"`
	Version   string       `json:"version,omitempty"`
	LoggedIn  LoginState   `json:"logged_in"`
	Evidence  []string     `json:"evidence,omitempty"` // file names / env var names checked, never values
	Caps      Capabilities `json:"capabilities"`
	Error     string       `json:"error,omitempty"`
}

// Level is the authority a turn runs with. It is a hint for native sandboxes
// (defense in depth); the Plexus policy callback is the real gate.
type Level int

const (
	LevelChat     Level = iota // no tools
	LevelReadOnly              // read files inside the workdir
	LevelFull                  // everything the harness can do
)

func (l Level) String() string {
	switch l {
	case LevelChat:
		return "chat"
	case LevelReadOnly:
		return "readonly"
	case LevelFull:
		return "full"
	}
	return fmt.Sprintf("level(%d)", int(l))
}

// ParseLevel parses "chat", "readonly" or "full".
func ParseLevel(s string) (Level, error) {
	switch s {
	case "chat", "":
		return LevelChat, nil
	case "readonly", "read-only":
		return LevelReadOnly, nil
	case "full":
		return LevelFull, nil
	}
	return LevelChat, fmt.Errorf("unknown level %q (want chat, readonly or full)", s)
}

// SessionOptions configures one conversation.
type SessionOptions struct {
	Workdir  string
	Persona  string   // appended system prompt / developer instructions
	ResumeID string   // native session id to resume, if any
	Exe      string   // executable override (default: detected path)
	Args     []string // extra args appended to the launch command
	Env      []string // extra environment entries (KEY=VALUE)
	// HostTools are mounted natively where the harness supports it
	// (Claude in-process MCP, Codex dynamicTools, DSH bridge).
	HostTools []ToolSpec
}

// Steerer is implemented by sessions that can fold a new message into the
// running turn (Claude queued input, Codex turn/steer, DSH bridge steer).
type Steerer interface {
	Steer(ctx context.Context, text string) error
}

// TaskStopper is implemented by sessions that can stop single background
// tasks (Claude stop_task). Stop uses it before the interrupt.
type TaskStopper interface {
	BackgroundTasks() []string
	StopTask(ctx context.Context, id string) error
}

// Turn is one user message.
type Turn struct {
	Text  string
	Level Level
}

// Session is one live conversation with a harness process.
type Session interface {
	// ID returns the native session id ("" until the harness reports one).
	ID() string
	// Events is the session-wide stream: it carries every turn's events
	// and anything that outlives a turn. Closed when the session ends.
	Events() <-chan Event
	// Send starts a turn and returns its id. Only one turn runs at a time.
	// Cancelling ctx interrupts the turn.
	Send(ctx context.Context, t Turn) (turnID string, err error)
	// Interrupt asks the harness to stop the running turn.
	Interrupt(ctx context.Context) error
	// Control is a raw native passthrough (e.g. a Claude control_request
	// subtype, a Codex or ACP JSON-RPC method). Owner-only in Plexus.
	Control(ctx context.Context, name string, payload json.RawMessage) (json.RawMessage, error)
	// Close stops the harness process tree.
	Close() error
}

// Harness is the interface every adapter implements.
type Harness interface {
	Name() string
	Capabilities() Capabilities
	// Detect must be safe: only --version/help style commands and local file
	// existence checks. It must never start a model request.
	Detect(ctx context.Context, env Env) DetectionResult
	StartSession(ctx context.Context, opts SessionOptions) (Session, error)
}

var (
	regMu    sync.RWMutex
	registry = map[string]Harness{}
)

// Register adds an adapter. Adapters call it from init().
func Register(h Harness) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[h.Name()]; dup {
		panic("harness: duplicate adapter " + h.Name())
	}
	registry[h.Name()] = h
}

// Get returns a registered adapter by name.
func Get(name string) (Harness, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	h, ok := registry[name]
	return h, ok
}

// All returns every registered adapter sorted by name.
func All() []Harness {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]Harness, 0, len(registry))
	for _, h := range registry {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// DetectAll runs Detect on every registered adapter.
func DetectAll(ctx context.Context, env Env) []DetectionResult {
	var out []DetectionResult
	for _, h := range All() {
		r := h.Detect(ctx, env)
		if IsShim(r.Path) {
			r.Error = "only an npm .cmd shim was found; Plexus will not spawn it (set the exe path to the real .exe or .js)"
		}
		out = append(out, r)
	}
	return out
}

// ErrActiveElsewhere is returned (wrapped) by StartSession when the native
// session to resume is open in another process: a desktop app, a terminal
// CLI, or another agent. Plexus then does not resume it.
var ErrActiveElsewhere = errors.New("this session is open in another app")

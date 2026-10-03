package harness

import "encoding/json"

// EventKind is the normalized event type every adapter emits.
type EventKind string

const (
	EventTextDelta  EventKind = "text_delta"  // streamed, provisional assistant text
	EventMessage    EventKind = "message"     // committed assistant message (authoritative text)
	EventToolUse    EventKind = "tool_use"    // a tool call started (informational)
	EventToolResult EventKind = "tool_result" // a tool call finished (informational)
	EventPermission EventKind = "permission"  // may this tool call run? reply via Decide
	EventQuestion   EventKind = "question"    // the harness asks the user; reply via Answer
	EventBackground EventKind = "background"  // background job lifecycle; may outlive its turn
	EventFinal      EventKind = "final"       // the turn finished; Text is the committed answer
	EventError      EventKind = "error"       // the turn (TurnID set) or session (TurnID "") failed
	EventExtension  EventKind = "extension"   // native-only event: Name + Raw, for passthrough
	EventHostTool   EventKind = "host_tool"   // the agent called a Plexus host tool; reply via Call
)

// ToolKind is the coarse class policy decisions are made on.
type ToolKind string

const (
	ToolRead  ToolKind = "read"  // read a file / search
	ToolWrite ToolKind = "write" // create, edit, delete or move files
	ToolShell ToolKind = "shell" // run a command
	ToolFetch ToolKind = "fetch" // network access (WebFetch, WebSearch, ...)
	ToolAsk   ToolKind = "ask"   // ask the user something
	ToolMeta  ToolKind = "meta"  // planning / todo bookkeeping with no side effects
	ToolOther ToolKind = "other" // MCP tools, sub-agents, anything unknown
)

// ToolRequest is one native tool call, normalized.
type ToolRequest struct {
	CallID  string   `json:"call_id,omitempty"` // native tool call id
	Name    string   `json:"name"`              // native tool name, e.g. "Bash", "commandExecution"
	Kind    ToolKind `json:"kind"`
	Paths   []string `json:"paths,omitempty"`   // file paths the call touches
	Command string   `json:"command,omitempty"` // shell command, if any
	Deletes []string `json:"deletes,omitempty"` // the subset of Paths the call deletes (or moves away)
	Input   any      `json:"input,omitempty"`   // raw native input (display only)
}

// OptionKind classifies a native permission option (ACP vocabulary).
type OptionKind string

const (
	AllowOnce    OptionKind = "allow_once"
	AllowAlways  OptionKind = "allow_always"
	RejectOnce   OptionKind = "reject_once"
	RejectAlways OptionKind = "reject_always"
)

// PermissionOption is one native choice offered with a permission request.
type PermissionOption struct {
	ID    string     `json:"id"`
	Label string     `json:"label"`
	Kind  OptionKind `json:"kind"`
}

// PermissionRequest is the full shape of a permission prompt.
type PermissionRequest struct {
	Tool    ToolRequest        `json:"tool"`
	Reason  string             `json:"reason,omitempty"`  // why the harness asks, if it says
	Options []PermissionOption `json:"options,omitempty"` // native options; empty = plain allow/deny
}

// Decision answers a PermissionRequest.
type Decision struct {
	Allow        bool
	OptionID     string // chosen native option, if any
	Always       bool   // remember for the session (only if the harness supports it)
	Reason       string
	UpdatedInput any // optional rewritten tool input (Claude Code supports this)
}

// QuestionOption is one choice of a question.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Question is one question the harness asks the user.
type Question struct {
	ID       string           `json:"id"`
	Header   string           `json:"header,omitempty"`
	Text     string           `json:"text"`
	Options  []QuestionOption `json:"options,omitempty"`
	Multi    bool             `json:"multi,omitempty"`
	FreeText bool             `json:"free_text,omitempty"`
}

// Answers maps question ID to the chosen labels / typed text. A nil map
// means the user declined or did not answer in time.
type Answers map[string][]string

// Event is one normalized harness event.
//
// IDs are stable for the life of a session. ParentID gives subagent
// lineage ("" = the root agent). TurnID is "" for events not tied to a turn,
// e.g. a background job that finishes after its turn ended.
type Event struct {
	ID        string             `json:"id"`
	ParentID  string             `json:"parent_id,omitempty"`
	TurnID    string             `json:"turn_id,omitempty"`
	Kind      EventKind          `json:"kind"`
	Text      string             `json:"text,omitempty"`
	Name      string             `json:"name,omitempty"`   // extension / job name
	Status    string             `json:"status,omitempty"` // background / tool status
	Tool      *ToolRequest       `json:"tool,omitempty"`
	Perm      *PermissionRequest `json:"permission,omitempty"`
	Questions []Question         `json:"questions,omitempty"`
	SessionID string             `json:"session_id,omitempty"`
	Raw       json.RawMessage    `json:"raw,omitempty"` // native message, untouched

	// Exactly one is set for EventPermission / EventQuestion and must be
	// called once. Unanswered requests become deny when the turn or the
	// session ends.
	Decide func(Decision) `json:"-"`
	Answer func(Answers)  `json:"-"`
	// Call answers an EventHostTool (Name = tool, Tool.Input = arguments).
	Call func(HostResult) `json:"-"`
}

// HostResult is the answer to a host tool call.
type HostResult struct {
	Text    string `json:"text"`
	IsError bool   `json:"is_error,omitempty"`
}

// ToolSpec declares a host tool Plexus offers to the agent.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

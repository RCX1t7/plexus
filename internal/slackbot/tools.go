package slackbot

import (
	"encoding/json"
	"fmt"

	"github.com/RCX1t7/plexus/internal/harness"
)

func jsonMarshal(v any) ([]byte, error)  { return json.Marshal(v) }
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
func schema(s string) json.RawMessage     { return json.RawMessage(s) }

const strList = `{"type":"array","items":{"type":"string"}}`

// HostTools are the optional Plexus tools mounted into harnesses that
// support it. They are ordinary tool calls with structured arguments; no
// harness has to produce a structured final report.
var HostTools = []harness.ToolSpec{
	{Name: "plexus_post", Description: "Post a message to the current Slack thread (use it to speak up in turns not addressed to you, or to report progress).",
		InputSchema: schema(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)},
	{Name: "plexus_delegate", Description: "Hand work to another Plexus partner in this thread with a handoff record. Write done_when before the work starts.",
		InputSchema: schema(`{"type":"object","properties":{"to":{"type":"string","description":"partner name or Slack user id"},` +
			`"task":{"type":"string","description":"one line"},"inputs":` + strList + `,"done_when":{"type":"string"},` +
			`"evidence":` + strList + `,"tried_failed":` + strList + `,"owner_if_stuck":{"type":"string"}},"required":["to","task","done_when"]}`)},
	{Name: "plexus_deliver", Description: "Post the delivery of finished work: summary, artifacts (paths under the workdir, hashed with SHA-256) and evidence.",
		InputSchema: schema(`{"type":"object","properties":{"summary":{"type":"string"},"artifacts":` + strList + `,"evidence":` + strList + `},"required":["summary"]}`)},
	{Name: "plexus_stop_tree", Description: "Stop the whole task tree of this thread (all partners). Only works when Sin asked for it in the message you are handling.",
		InputSchema: schema(`{"type":"object","properties":{}}`)},
}

func toolArgs(ev harness.Event, v any) error {
	if ev.Tool == nil {
		return fmt.Errorf("missing arguments")
	}
	b, err := json.Marshal(ev.Tool.Input)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

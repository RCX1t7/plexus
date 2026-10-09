package slackbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// SlackPoster posts with chat.postMessage and classifies failures.
type SlackPoster struct{ API *slack.Client }

const metaEvent = "plexus_post"

func (p SlackPoster) Post(ctx context.Context, channel, thread, text string, meta Meta) (string, error) {
	opts := []slack.MsgOption{slack.MsgOptionText(text, false), slack.MsgOptionMetadata(slack.SlackMetadata{
		EventType: metaEvent, EventPayload: map[string]any{"request_id": meta.RequestID, "kind": meta.Kind}})}
	if len(meta.Blocks) > 0 {
		var b slack.Blocks
		if err := json.Unmarshal(meta.Blocks, &b); err == nil {
			opts = append(opts, slack.MsgOptionBlocks(b.BlockSet...))
		}
	}
	if thread != "" {
		opts = append(opts, slack.MsgOptionTS(thread))
	}
	_, ts, err := p.API.PostMessageContext(ctx, channel, opts...)
	if err == nil {
		return ts, nil
	}
	var rl *slack.RateLimitedError
	var se slack.SlackErrorResponse
	switch {
	case errors.As(err, &rl):
		return "", &PostError{Outcome: NotSent, RetryAfter: rl.RetryAfter, Err: err}
	case errors.As(err, &se):
		return "", &PostError{Outcome: Rejected, Err: err} // Slack replied: definitely not posted
	}
	return "", &PostError{Outcome: Uncertain, Err: err}
}

// Update replaces a post's text and drops its blocks (buttons).
func (p SlackPoster) Update(ctx context.Context, channel, ts, text string) error {
	_, _, _, err := p.API.UpdateMessageContext(ctx, channel, ts, slack.MsgOptionText(text, false), slack.MsgOptionBlocks())
	return err
}

// Find looks for requestID in the metadata of the thread's replies (or the
// channel's recent history for top-level posts).
func (p SlackPoster) Find(ctx context.Context, channel, thread, requestID string) (string, bool, error) {
	match := func(ms []slack.Message) (string, bool) {
		for _, m := range ms {
			if m.Metadata.EventType == metaEvent && fmt.Sprint(m.Metadata.EventPayload["request_id"]) == requestID {
				return m.Timestamp, true
			}
		}
		return "", false
	}
	cursor := ""
	for page := 0; page < 10; page++ {
		var ms []slack.Message
		var more bool
		var err error
		if thread != "" {
			ms, more, cursor, err = p.API.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
				ChannelID: channel, Timestamp: thread, Cursor: cursor, Limit: 200, IncludeAllMetadata: true})
		} else {
			var r *slack.GetConversationHistoryResponse
			r, err = p.API.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
				ChannelID: channel, Cursor: cursor, Limit: 200, IncludeAllMetadata: true})
			if r != nil {
				ms, more, cursor = r.Messages, r.HasMore, r.ResponseMetaData.NextCursor
			}
		}
		if err != nil {
			return "", false, err
		}
		if ts, ok := match(ms); ok {
			return ts, true, nil
		}
		if !more || cursor == "" {
			return "", false, nil
		}
	}
	return "", false, errors.New("history too long to reconcile")
}

// Connect verifies the tokens (auth.test) and returns the API client and
// this bot's Slack user id. apiURL overrides the Web API base (tests).
func Connect(ctx context.Context, botToken, appToken, apiURL string) (*slack.Client, string, error) {
	opts := []slack.Option{slack.OptionAppLevelToken(appToken)}
	if apiURL != "" {
		if !strings.HasSuffix(apiURL, "/") {
			apiURL += "/"
		}
		opts = append(opts, slack.OptionAPIURL(apiURL))
	}
	api := slack.New(botToken, opts...)
	r, err := api.AuthTestContext(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("auth.test: %w", err)
	}
	return api, r.UserID, nil
}

// Run keeps one Socket Mode connection open and feeds messages to w.
// slack-go reconnects on its own; Run returns when ctx ends or the
// connection fails for good (the supervisor then restarts it with backoff).
// onConnected (optional) runs each time the WebSocket is connected.
func Run(ctx context.Context, api *slack.Client, w *Worker, onConnected func()) error {
	sm := socketmode.New(api)
	errc := make(chan error, 1)
	go func() { errc <- sm.RunContext(ctx) }()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errc:
			return err
		case evt, ok := <-sm.Events:
			if !ok {
				return errors.New("socket mode closed")
			}
			switch evt.Type {
			case socketmode.EventTypeConnected:
				if onConnected != nil {
					onConnected()
				}
			case socketmode.EventTypeConnectionError, socketmode.EventTypeInvalidAuth:
				// slack-go retries on its own; make the failures visible.
				var detail string
				if ce, ok := evt.Data.(*slack.ConnectionErrorEvent); ok && ce.ErrorObj != nil {
					detail = ce.ErrorObj.Error()
				}
				w.log().Warn("slack connection problem", "event", string(evt.Type), "err", detail)
			}
			if evt.Type == socketmode.EventTypeInteractive {
				if evt.Request != nil {
					sm.Ack(*evt.Request) // ack first: Slack wants it within 3 s
				}
				if cb, ok := evt.Data.(slack.InteractionCallback); ok && cb.Type == slack.InteractionTypeBlockActions {
					for _, a := range cb.ActionCallback.BlockActions {
						if mode := ModeForAction(a.ActionID); mode != "" {
							w.Approve(ctx, a.Value, cb.User.ID, mode)
						}
					}
				}
				continue
			}
			if evt.Type != socketmode.EventTypeEventsAPI {
				continue
			}
			ea, ok := evt.Data.(slackevents.EventsAPIEvent)
			if !ok {
				if evt.Request != nil {
					sm.Ack(*evt.Request)
				}
				continue
			}
			if m, ok := ea.InnerEvent.Data.(*slackevents.MessageEvent); ok {
				if m.SubType != "" && m.SubType != "bot_message" && m.SubType != "thread_broadcast" {
					if evt.Request != nil {
						sm.Ack(*evt.Request)
					}
					continue // edits, deletes, joins ...
				}
				user := m.User
				if user == "" && m.Message != nil {
					user = m.Message.User
				}
				// Handle persists the dedup record (and queues the work)
				// before the ack, so a crash in between means a redelivery,
				// not a lost message. It returns quickly.
				var atts []slack.Attachment
				if m.Message != nil {
					atts = m.Message.Attachments
				}
				w.Handle(ctx, Inbound{Channel: m.Channel, TS: m.TimeStamp, ThreadTS: m.ThreadTimeStamp,
					User: user, Text: m.Text, DM: m.ChannelType == "im", Quoted: quotedOf(atts)})
			}
			if evt.Request != nil {
				sm.Ack(*evt.Request)
			}
		}
	}
}

// quotedOf extracts shared / forwarded message bodies from attachments.
func quotedOf(atts []slack.Attachment) []Quoted {
	var out []Quoted
	for _, a := range atts {
		text := a.Text
		if text == "" {
			text = a.Fallback
		}
		if a.Pretext != "" {
			text = strings.TrimSpace(a.Pretext + "\n" + text)
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		src := "forwarded message"
		if a.AuthorName != "" {
			src = "forwarded from " + a.AuthorName
		}
		if a.FromURL != "" {
			src += " (" + a.FromURL + ")"
		}
		out = append(out, Quoted{AuthorID: a.AuthorID, Source: src, Text: text})
	}
	return out
}

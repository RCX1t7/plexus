// Package slackbot connects one partner (Slack App) to one harness: Socket
// Mode in, persistent outbox out, policy in between.
package slackbot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/RCX1t7/plexus/internal/redact"
	"github.com/RCX1t7/plexus/internal/store"
)

// Meta rides in the Slack message metadata, so a post can be found again
// after a crash (reconciliation) without trusting local state.
type Meta struct {
	RequestID string `json:"request_id"`
	Kind      string `json:"kind"`
}

// Poster posts to Slack and can look a post up again by request id.
type Poster interface {
	Post(ctx context.Context, channel, threadTS, text string, meta Meta) (ts string, err error)
	// Find searches the thread (or channel) history for a message carrying
	// requestID in its metadata.
	Find(ctx context.Context, channel, threadTS, requestID string) (ts string, found bool, err error)
}

// Outcome classifies a failed post.
type Outcome int

const (
	NotSent   Outcome = iota // definitely not delivered, safe to retry (e.g. rate limited)
	Rejected                 // Slack answered with an error: terminal failure
	Uncertain                // network error / timeout: may or may not have posted
)

// PostError carries the classification.
type PostError struct {
	Outcome    Outcome
	RetryAfter time.Duration
	Err        error
}

func (e *PostError) Error() string { return e.Err.Error() }
func (e *PostError) Unwrap() error { return e.Err }

func classify(err error) (Outcome, time.Duration) {
	var pe *PostError
	if errors.As(err, &pe) {
		return pe.Outcome, pe.RetryAfter
	}
	return Uncertain, 0
}

// RequestID derives a stable id, so a replayed event (Slack retry, restart)
// maps to the same outbox row and is never posted twice.
func RequestID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

const chunkSize = 3500

// Chunks splits text on rune boundaries, preferring newlines.
func Chunks(text string) []string {
	var out []string
	for len(text) > chunkSize {
		cut := chunkSize
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		for i := cut; i > chunkSize/2; i-- {
			if text[i] == '\n' {
				cut = i + 1
				break
			}
		}
		out = append(out, text[:cut])
		text = text[cut:]
	}
	if text != "" || len(out) == 0 {
		out = append(out, text)
	}
	return out
}

// Outbox persists and delivers messages for one bot.
type Outbox struct {
	Bot    string
	Store  *store.Store
	Poster Poster
	// Secrets are masked in every outbound message in addition to the
	// generic redaction patterns.
	Secrets []string
}

// Post is one outbound message.
type Post struct {
	ID, Channel, Thread, Text string
	Kind                      string // reply, post, handoff, deliver, stop_ack, ask, warn
	Origin                    string // trust source of the turn that wrote it
	HandoffID                 string
}

// Enqueue stores a post (chunked) under its request id. Duplicates are
// ignored, which is also how exactly one stop_ack is posted per stop.
func (o *Outbox) Enqueue(p Post) ([]string, error) {
	var ids []string
	for i, c := range Chunks(p.Text) {
		id := p.ID
		if i > 0 {
			id = fmt.Sprintf("%s#%d", p.ID, i)
		}
		m := store.Msg{RequestID: id, Bot: o.Bot, Channel: p.Channel, ThreadTS: p.Thread, Text: c,
			Kind: p.Kind, Origin: p.Origin}
		if i == 0 {
			m.HandoffID = p.HandoffID
		}
		fresh, err := o.Store.Enqueue(m)
		if err != nil {
			return ids, err
		}
		if fresh {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (o *Outbox) sent(m store.Msg, ts string) (string, error) {
	if err := o.Store.Finish(m.RequestID, store.Sent, ts, ""); err != nil {
		return "", err
	}
	return store.Sent, o.Store.PutOrigin(m.Channel, ts, store.Origin{Bot: o.Bot, Source: m.Origin, Handoff: m.HandoffID})
}

// Deliver attempts one pending message. A send with an unknown outcome is
// marked uncertain, to be reconciled later; it is never blindly resent.
func (o *Outbox) Deliver(ctx context.Context, id string) (string, error) {
	won, err := o.Store.Claim(id)
	if err != nil {
		return "", err
	}
	m, err := o.Store.Get(id)
	if err != nil || !won {
		return m.State, err // already handled (or being handled) elsewhere
	}
	ts, perr := o.Poster.Post(ctx, m.Channel, m.ThreadTS, redact.String(m.Text, o.Secrets...), Meta{RequestID: id, Kind: m.Kind})
	if perr == nil {
		return o.sent(m, ts)
	}
	reason := redact.String(perr.Error(), o.Secrets...)
	switch out, _ := classify(perr); out {
	case NotSent:
		return store.Pending, o.Store.Release(id, reason)
	case Rejected:
		return store.Failed, o.Store.Finish(id, store.Failed, "", reason)
	default:
		return store.Uncertain, o.Store.Finish(id, store.Uncertain, "", reason)
	}
}

// Reconcile resolves uncertain messages against Slack history: a message
// found by its request id is marked sent; one proven absent is requeued
// and delivered; if history cannot be read it stays uncertain.
func (o *Outbox) Reconcile(ctx context.Context) (resolved, resent int, err error) {
	ids, err := o.Store.UncertainIDs(o.Bot)
	if err != nil {
		return 0, 0, err
	}
	for _, id := range ids {
		m, err := o.Store.Get(id)
		if err != nil {
			continue
		}
		ts, found, ferr := o.Poster.Find(ctx, m.Channel, m.ThreadTS, id)
		switch {
		case ferr != nil:
			continue // unknown: keep it uncertain, try again next start
		case found:
			if _, err := o.sent(m, ts); err != nil {
				return resolved, resent, err
			}
			resolved++
		default:
			if ok, _ := o.Store.Requeue(id); ok {
				if _, err := o.Deliver(ctx, id); err != nil {
					return resolved, resent, err
				}
				resent++
			}
		}
	}
	return resolved, resent, nil
}

// Flush delivers every pending message in order, waiting out rate limits.
func (o *Outbox) Flush(ctx context.Context) error {
	ids, err := o.Store.PendingIDs(o.Bot)
	if err != nil {
		return err
	}
	for _, id := range ids {
		for {
			state, err := o.Deliver(ctx, id)
			if err != nil {
				return err
			}
			if state != store.Pending {
				break
			}
			select { // rate limited: wait, then retry the same row
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}

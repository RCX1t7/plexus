// Package slackc is a minimal Slack Web API + Socket Mode client used by the
// fake self-tests and by the reference stub hub. Not production code.
package slackc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/RCX1t7/plexus/tests/internal/wsmini"
)

type Client struct {
	APIURL string // ends with "/"
	HTTP   *http.Client
}

func New(apiURL string) *Client {
	if !strings.HasSuffix(apiURL, "/") {
		apiURL += "/"
	}
	return &Client{APIURL: apiURL, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// Call invokes a Web API method with a JSON body. It retries 429 responses
// honoring Retry-After (max 3 attempts).
func (c *Client) Call(method, token string, params map[string]any) (map[string]any, error) {
	body, _ := json.Marshal(params)
	for attempt := 0; ; attempt++ {
		req, _ := http.NewRequest("POST", c.APIURL+method, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		var out map[string]any
		err = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if resp.StatusCode == 429 && attempt < 3 {
			time.Sleep(time.Second)
			continue
		}
		if err != nil {
			return nil, err
		}
		if ok, _ := out["ok"].(bool); !ok {
			return out, fmt.Errorf("slack %s: %v", method, out["error"])
		}
		return out, nil
	}
}

// Envelope is a Socket Mode envelope.
type Envelope struct {
	EnvelopeID   string         `json:"envelope_id"`
	Type         string         `json:"type"`
	RetryAttempt int            `json:"retry_attempt"`
	Payload      map[string]any `json:"payload"`
	Reason       string         `json:"reason"`
}

// Socket is an open Socket Mode connection.
type Socket struct{ c *wsmini.Conn }

// OpenSocket calls apps.connections.open and dials the returned URL.
func (c *Client) OpenSocket(appToken string) (*Socket, error) {
	out, err := c.Call("apps.connections.open", appToken, map[string]any{})
	if err != nil {
		return nil, err
	}
	u, _ := out["url"].(string)
	conn, err := wsmini.Dial(u)
	if err != nil {
		return nil, err
	}
	return &Socket{c: conn}, nil
}

// Next returns the next envelope (hello/disconnect/events_api ...).
func (s *Socket) Next() (Envelope, error) {
	_, b, err := s.c.ReadMessage()
	if err != nil {
		return Envelope{}, err
	}
	var e Envelope
	err = json.Unmarshal(b, &e)
	return e, err
}

// Ack acknowledges an envelope.
func (s *Socket) Ack(id string) error {
	b, _ := json.Marshal(map[string]string{"envelope_id": id})
	return s.c.WriteText(b)
}

func (s *Socket) Close() error { return s.c.Close() }

// Event extracts payload.event from an events_api envelope.
func (e Envelope) Event() map[string]any {
	ev, _ := e.Payload["event"].(map[string]any)
	return ev
}

func (e Envelope) EventID() string { s, _ := e.Payload["event_id"].(string); return s }

// Package wsmini is a tiny, dependency-free RFC 6455 WebSocket implementation
// (text/binary/ping/pong/close, fragmentation on read). It exists only so the
// fake Slack Socket Mode server and the reference stub hub need no third-party
// modules. It is test infrastructure, not production code.
package wsmini

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	OpCont   = 0x0
	OpText   = 0x1
	OpBinary = 0x2
	OpClose  = 0x8
	OpPing   = 0x9
	OpPong   = 0xA
)

// Conn is a WebSocket connection (client or server side).
type Conn struct {
	c       net.Conn
	r       *bufio.Reader
	client  bool
	wmu     sync.Mutex
	closeMu sync.Mutex
	closed  bool
}

func acceptKey(k string) string {
	h := sha1.Sum([]byte(k + guid))
	return base64.StdEncoding.EncodeToString(h[:])
}

// Upgrade performs the server side handshake.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "not a websocket request", http.StatusBadRequest)
		return nil, errors.New("wsmini: missing Upgrade: websocket")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return nil, errors.New("wsmini: missing Sec-WebSocket-Key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("wsmini: response writer cannot hijack")
	}
	nc, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n"
	if _, err := brw.WriteString(resp); err != nil {
		nc.Close()
		return nil, err
	}
	if err := brw.Flush(); err != nil {
		nc.Close()
		return nil, err
	}
	return &Conn{c: nc, r: brw.Reader}, nil
}

// Dial opens a client connection to a ws:// URL (wss is not supported; the
// fakes only ever listen on loopback).
func Dial(raw string) (*Conn, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "ws" {
		return nil, fmt.Errorf("wsmini: unsupported scheme %q", u.Scheme)
	}
	nc, err := net.Dial("tcp", u.Host)
	if err != nil {
		return nil, err
	}
	kb := make([]byte, 16)
	_, _ = rand.Read(kb)
	key := base64.StdEncoding.EncodeToString(kb)
	path := u.RequestURI()
	req := "GET " + path + " HTTP/1.1\r\nHost: " + u.Host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(nc, req); err != nil {
		nc.Close()
		return nil, err
	}
	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		nc.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) {
		nc.Close()
		return nil, fmt.Errorf("wsmini: bad handshake: %s", resp.Status)
	}
	return &Conn{c: nc, r: br, client: true}, nil
}

func (c *Conn) writeFrame(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	hdr := make([]byte, 0, 14)
	hdr = append(hdr, 0x80|op)
	mask := byte(0)
	if c.client {
		mask = 0x80
	}
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, mask|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, mask|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, mask|127)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		hdr = append(hdr, b[:]...)
	}
	data := payload
	if c.client {
		var mk [4]byte
		_, _ = rand.Read(mk[:])
		hdr = append(hdr, mk[:]...)
		data = make([]byte, n)
		for i := range payload {
			data[i] = payload[i] ^ mk[i%4]
		}
	}
	if _, err := c.c.Write(hdr); err != nil {
		return err
	}
	_, err := c.c.Write(data)
	return err
}

// WriteText sends one text message.
func (c *Conn) WriteText(b []byte) error { return c.writeFrame(OpText, b) }

func (c *Conn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.r, h[:]); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	op = h[0] & 0x0F
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.r, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.r, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > 64<<20 {
		err = errors.New("wsmini: frame too large")
		return
	}
	var mk [4]byte
	if masked {
		if _, err = io.ReadFull(c.r, mk[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.r, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mk[i%4]
		}
	}
	return
}

// ReadMessage returns the next data message, answering pings transparently.
// It returns io.EOF after a close frame.
func (c *Conn) ReadMessage() (byte, []byte, error) {
	var msgOp byte
	var buf []byte
	for {
		fin, op, p, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case OpPing:
			_ = c.writeFrame(OpPong, p)
			continue
		case OpPong:
			continue
		case OpClose:
			_ = c.writeFrame(OpClose, p)
			c.Close()
			return 0, nil, io.EOF
		case OpCont:
			buf = append(buf, p...)
		default:
			msgOp = op
			buf = append([]byte(nil), p...)
		}
		if fin {
			return msgOp, buf, nil
		}
	}
}

// Ping sends a ping frame.
func (c *Conn) Ping() error { return c.writeFrame(OpPing, nil) }

// CloseGracefully sends a close frame then closes the socket.
func (c *Conn) CloseGracefully() error {
	_ = c.writeFrame(OpClose, []byte{0x03, 0xE8})
	return c.Close()
}

// Close closes the underlying connection (idempotent).
func (c *Conn) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.c.Close()
}

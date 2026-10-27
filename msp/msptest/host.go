// Package msptest is an in-memory MSP host for tests: it answers the
// methods a test registers, records every message the client sends, and
// can replay a recorded `muse serve` session so tests run against real
// Muse output without Muse or a model account.
package msptest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rustambekabdusalomov/agnts-muse/msp"
)

// Message is a JSON-RPC message the client sent.
type Message struct {
	ID     *json.RawMessage `json:"id,omitempty"`
	Method string           `json:"method,omitempty"`
	Params json.RawMessage  `json:"params,omitempty"`
	Result json.RawMessage  `json:"result,omitempty"`
}

// HandlerFunc answers one request. Returning an *msp.Error sends an error
// response.
type HandlerFunc func(params json.RawMessage) (any, error)

// Host is a fake `muse serve`.
type Host struct {
	in  *io.PipeReader // client → host
	out *io.PipeWriter // host → client

	wmu      sync.Mutex
	mu       sync.Mutex
	handlers map[string]HandlerFunc
	received []Message
	closed   chan struct{}
}

// conn is the client's end.
type conn struct {
	io.Reader
	io.Writer
	close func() error
}

func (c conn) Close() error { return c.close() }

// New starts a host and returns the connection a client should use.
// Initialize is answered by default.
func New() (*Host, io.ReadWriteCloser) {
	cr, hw := io.Pipe() // host writes, client reads
	hr, cw := io.Pipe() // client writes, host reads
	h := &Host{in: hr, out: hw, handlers: map[string]HandlerFunc{}, closed: make(chan struct{})}
	h.Handle(msp.MethodInitialize, func(json.RawMessage) (any, error) {
		return msp.InitializeResult{ServerInfo: msp.ServerInfo{Name: "muse", Version: "test"},
			Schema: msp.SchemaInfo{Version: msp.SchemaVersion, Fingerprint: msp.SchemaFingerprint}, PlatformOS: "linux"}, nil
	})
	go h.serve()
	c := conn{Reader: cr, Writer: cw, close: func() error {
		_ = cw.Close()
		return cr.Close()
	}}
	return h, c
}

// Handle registers the answer to method, replacing any earlier one.
func (h *Host) Handle(method string, fn HandlerFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handlers[method] = fn
}

// Notify sends a notification to the client.
func (h *Host) Notify(method string, params any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return h.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": json.RawMessage(b)})
}

// NotifyRaw sends a notification whose params are already JSON.
func (h *Host) NotifyRaw(n msp.Notification) error {
	return h.write(map[string]any{"jsonrpc": "2.0", "method": n.Method, "params": n.Params})
}

// Request sends a server request (approval/request, …) with the given id.
func (h *Host) Request(id int, method string, params any) error {
	return h.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

// Received returns what the client sent so far.
func (h *Host) Received() []Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Message(nil), h.received...)
}

// Sent returns the params of every call or notification of method.
func (h *Host) Sent(method string) []json.RawMessage {
	var out []json.RawMessage
	for _, m := range h.Received() {
		if m.Method == method {
			out = append(out, m.Params)
		}
	}
	return out
}

// WaitFor blocks until the client sent method, or ctx ends.
func (h *Host) WaitFor(ctx context.Context, method string) (json.RawMessage, error) {
	for {
		if s := h.Sent(method); len(s) > 0 {
			return s[len(s)-1], nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("msptest: %s never sent: %w", method, ctx.Err())
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// Close ends the connection, as if `muse serve` exited.
func (h *Host) Close() error {
	_ = h.in.Close()
	return h.out.Close()
}

func (h *Host) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	h.wmu.Lock()
	defer h.wmu.Unlock()
	_, err = h.out.Write(append(b, '\n'))
	return err
}

func (h *Host) serve() {
	defer close(h.closed)
	sc := bufio.NewScanner(h.in)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	for sc.Scan() {
		var m Message
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		h.mu.Lock()
		h.received = append(h.received, m)
		fn := h.handlers[m.Method]
		h.mu.Unlock()
		if m.ID == nil || m.Method == "" {
			continue // a notification or a response to our request
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": m.ID}
		if fn == nil {
			resp["error"] = &msp.Error{Code: -32601, Message: "not handled by msptest: " + m.Method, Kind: "methodNotFound"}
		} else if res, err := fn(m.Params); err != nil {
			resp["error"] = err
		} else {
			resp["result"] = res
		}
		_ = h.write(resp)
	}
}

// Recording is the server side of a recorded session: the notifications
// `muse serve` sent, in order.
type Recording []msp.Notification

// Load reads a recording made by msp.WithTrace (or the probe scripts):
// lines starting "< " are server messages, "> " client messages.
// Responses and client lines are skipped.
func Load(path string) (Recording, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec Recording
	for line := range bytes.Lines(b) {
		s, ok := strings.CutPrefix(string(line), "< ")
		if !ok {
			continue
		}
		var m Message
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			return nil, fmt.Errorf("msptest: %s: %w", path, err)
		}
		if m.Method != "" && m.ID == nil {
			rec = append(rec, msp.Notification{Method: m.Method, Params: m.Params})
		}
	}
	return rec, nil
}

// Turn returns the notifications from turn/started through turn/completed.
func (r Recording) Turn() Recording {
	start, end := -1, -1
	for i, n := range r {
		if n.Method == msp.NoteTurnStarted && start < 0 {
			start = i
		}
		if n.Method == msp.NoteTurnCompleted {
			end = i
		}
	}
	if start < 0 || end < start {
		return nil
	}
	return r[start : end+1]
}

// Rewrite returns a copy with every occurrence of old replaced by new in
// the params, such as a recorded session id replaced with the test's.
func (r Recording) Rewrite(old, new string) Recording {
	out := make(Recording, len(r))
	for i, n := range r {
		out[i] = msp.Notification{Method: n.Method, Params: bytes.ReplaceAll(n.Params, []byte(old), []byte(new))}
	}
	return out
}

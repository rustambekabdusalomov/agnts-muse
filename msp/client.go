// Package msp is a client for MSP, the session protocol Muse Code serves
// over stdio (`muse serve`): JSON-RPC 2.0, one message per line.
//
// A Client correlates requests with responses, answers the server's own
// requests (approval/request, userInput/request) through a [Handler], and
// delivers notifications in order on [Client.Notifications]. Notifications
// are queued without bound rather than dropped or allowed to block the
// reader: a slow consumer must never stall the response to a Call it is
// waiting on, which a bounded channel would do (and deadlock).
//
// The types in this package are the subset of the MSP schema agnts-muse
// uses, checked field by field against the schema snapshot the Muse binary
// exports (testdata/msp.schema.json) by schema_test.go.
package msp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// DefaultMaxMessageBytes caps one line from the server. Tool outputs can be
// large; a line longer than this ends the connection with ErrTooLarge.
const DefaultMaxMessageBytes = 32 << 20

// Notification is a server notification.
type Notification struct {
	Method string
	Params json.RawMessage
}

// Decode unmarshals a notification's params.
func Decode[T any](n Notification) (T, error) {
	var v T
	if err := json.Unmarshal(n.Params, &v); err != nil {
		return v, fmt.Errorf("msp: decode %s: %w", n.Method, err)
	}
	return v, nil
}

// Handler answers a request the server sends to the client. The result is
// marshalled as the response; an *Error is sent as an error response.
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// Option configures a Client.
type Option func(*Client)

// WithHandler sets the handler for server requests. Without one, MSP's
// presentation requests (approval/request, userInput/request) are
// acknowledged with an empty receipt, since decisions travel as separate
// commands, and anything else gets methodNotFound.
func WithHandler(h Handler) Option { return func(c *Client) { c.handler = h } }

// WithMaxMessageBytes changes DefaultMaxMessageBytes.
func WithMaxMessageBytes(n int) Option { return func(c *Client) { c.maxLine = n } }

// WithTrace calls fn with every line sent ('>') and received ('<'), for
// recording sessions and debugging. fn must not retain line.
func WithTrace(fn func(dir byte, line []byte)) Option { return func(c *Client) { c.trace = fn } }

// Client is an MSP connection. Its methods are safe for concurrent use.
type Client struct {
	rw      io.ReadWriteCloser
	handler Handler
	maxLine int
	trace   func(byte, []byte)

	wmu sync.Mutex // serialises writes

	mu      sync.Mutex
	pending map[int64]chan response
	err     error // why the connection ended; set once
	nextID  atomic.Int64

	queue *queue
	out   chan Notification
	done  chan struct{}
	ctx   context.Context // cancelled when the connection ends
	stop  context.CancelFunc
}

type response struct {
	result json.RawMessage
	err    error
}

// wire is any JSON-RPC message.
type wire struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *Error           `json:"error,omitempty"`
}

// NewClient starts a client on rw, typically the stdin and stdout of
// `muse serve`. It owns rw: Close closes it.
func NewClient(rw io.ReadWriteCloser, opts ...Option) *Client {
	c := &Client{rw: rw, maxLine: DefaultMaxMessageBytes, pending: map[int64]chan response{},
		queue: newQueue(), out: make(chan Notification), done: make(chan struct{})}
	for _, o := range opts {
		o(c)
	}
	c.ctx, c.stop = context.WithCancel(context.Background())
	go c.read()
	go c.deliver()
	return c
}

// Notifications delivers server notifications in order. It is closed after
// the connection ends and every queued notification was delivered.
func (c *Client) Notifications() <-chan Notification { return c.out }

// Done is closed when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports why the connection ended (nil while it is open).
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close ends the connection.
func (c *Client) Close() error {
	err := c.rw.Close()
	c.fail(ErrClosed)
	return err
}

// Call sends a request and decodes its result into result (which may be
// nil). It returns an *Error for an error response, ErrClosed if the
// connection ends first, or ctx's error.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)
	ch := make(chan response, 1)
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	raw := json.RawMessage(fmt.Appendf(nil, "%d", id))
	if err := c.send(wire{JSONRPC: "2.0", ID: &raw, Method: method, Params: mustJSON(params)}); err != nil {
		return err
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if result == nil || len(r.result) == 0 {
			return nil
		}
		if err := json.Unmarshal(r.result, result); err != nil {
			return fmt.Errorf("msp: decode %s result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Notify sends a notification.
func (c *Client) Notify(method string, params any) error {
	return c.send(wire{JSONRPC: "2.0", Method: method, Params: mustJSON(params)})
}

func mustJSON(v any) json.RawMessage {
	if v == nil {
		return json.RawMessage("{}")
	}
	b, err := json.Marshal(v)
	if err != nil {
		// Params are this package's own types; failing to marshal them
		// is a programming error.
		panic(fmt.Sprintf("msp: marshal params: %v", err))
	}
	return b
}

func (c *Client) send(m wire) error {
	line, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if c.trace != nil {
		c.trace('>', line)
	}
	line = append(line, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.Err(); err != nil {
		return err
	}
	if _, err := c.rw.Write(line); err != nil {
		c.fail(fmt.Errorf("%w: %w", ErrClosed, err))
		return c.Err()
	}
	return nil
}

func (c *Client) read() {
	r := bufio.NewReaderSize(c.rw, 64<<10)
	for {
		line, err := readLine(r, c.maxLine)
		if len(bytes.TrimSpace(line)) > 0 {
			if c.trace != nil {
				c.trace('<', line)
			}
			c.dispatch(line)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = ErrClosed
			}
			c.fail(err)
			return
		}
	}
}

// readLine reads one newline-terminated line of at most max bytes.
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		frag, err := r.ReadSlice('\n')
		buf = append(buf, frag...)
		if len(buf) > max {
			return nil, fmt.Errorf("%w: line over %d bytes", ErrTooLarge, max)
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return bytes.TrimRight(buf, "\r\n"), err
		}
	}
}

func (c *Client) dispatch(line []byte) {
	var m wire
	if err := json.Unmarshal(line, &m); err != nil {
		// A line that is not JSON-RPC is the server's problem, not a
		// reason to drop the session; MSP never sends one in practice.
		return
	}
	switch {
	case m.ID != nil && m.Method != "":
		go c.answer(*m.ID, m.Method, m.Params)
		if m.Method == RequestApproval {
			// Muse asks for each later requirement of an approval only
			// with this request (the first also comes as
			// approval/requested). The receipt does not decide it, so it
			// reaches the caller like the notification; a decision is an
			// approval/decide command.
			c.queue.push(Notification{Method: NoteApprovalRequested, Params: m.Params})
		}
	case m.ID != nil:
		var id int64
		if json.Unmarshal(*m.ID, &id) != nil {
			return
		}
		c.mu.Lock()
		ch := c.pending[id]
		c.mu.Unlock()
		if ch == nil {
			return // the caller gave up (context cancelled)
		}
		r := response{result: m.Result}
		if m.Error != nil {
			r = response{err: m.Error}
		}
		select { // buffered; never block the reader on a duplicate id
		case ch <- r:
		default:
		}
	case m.Method != "":
		c.queue.push(Notification{Method: m.Method, Params: m.Params})
	}
}

// answer replies to a server request.
func (c *Client) answer(id json.RawMessage, method string, params json.RawMessage) {
	var result any
	var err error
	switch {
	case c.handler != nil:
		result, err = c.handler(c.ctx, method, params)
	case method == RequestApproval || method == RequestUserInput:
		result = struct{}{} // presentation receipt; decisions are commands
	default:
		err = &Error{Code: codeMethodNotFound, Message: "method not found: " + method, Kind: "methodNotFound"}
	}
	resp := wire{JSONRPC: "2.0", ID: &id}
	var rpcErr *Error
	switch {
	case errors.As(err, &rpcErr):
		resp.Error = rpcErr
	case err != nil:
		resp.Error = &Error{Code: codeInternal, Message: err.Error()}
	default:
		resp.Result = mustJSON(result)
	}
	_ = c.send(resp) // a closed connection has nobody to answer
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	pending := c.pending
	c.pending = map[int64]chan response{}
	c.mu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- response{err: err}:
		default: // already answered
		}
	}
	c.stop()
	c.queue.close()
	close(c.done)
}

func (c *Client) deliver() {
	defer close(c.out)
	for {
		n, ok := c.queue.pop()
		if !ok {
			return
		}
		c.out <- n
	}
}

// queue is an unbounded FIFO of notifications.
type queue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []Notification
	closed bool
}

func newQueue() *queue {
	q := &queue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *queue) push(n Notification) {
	q.mu.Lock()
	q.items = append(q.items, n)
	q.mu.Unlock()
	q.cond.Signal()
}

func (q *queue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast()
}

// pop waits for the next notification; ok is false once the queue is
// closed and empty.
func (q *queue) pop() (Notification, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.items) == 0 {
		return Notification{}, false
	}
	n := q.items[0]
	q.items[0] = Notification{}
	q.items = q.items[1:]
	return n, true
}

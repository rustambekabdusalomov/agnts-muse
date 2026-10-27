package msp_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rustambekabdusalomov/agnts-muse/msp"
	"github.com/rustambekabdusalomov/agnts-muse/msp/msptest"
)

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestInitializeHandshake(t *testing.T) {
	h, conn := msptest.New()
	c := msp.NewClient(conn)
	defer c.Close()
	res, err := c.Initialize(ctxT(t), msp.ClientInfo{Name: "agnts_muse", Version: "test"})
	if err != nil || res.ServerInfo.Name != "muse" {
		t.Fatalf("initialize = %+v, %v", res, err)
	}
	if _, err := h.WaitFor(ctxT(t), msp.NotificationInitialized); err != nil {
		t.Fatal("initialized notification not sent after the reply")
	}
}

func TestSchemaMismatch(t *testing.T) {
	h, conn := msptest.New()
	h.Handle(msp.MethodInitialize, func(json.RawMessage) (any, error) {
		return msp.InitializeResult{Schema: msp.SchemaInfo{Version: 2}}, nil
	})
	c := msp.NewClient(conn)
	defer c.Close()
	if _, err := c.Initialize(ctxT(t), msp.ClientInfo{Name: "x", Version: "1"}); !errors.Is(err, msp.ErrSchemaMismatch) {
		t.Fatalf("err = %v, want ErrSchemaMismatch", err)
	}
}

func TestErrorKinds(t *testing.T) {
	h, conn := msptest.New()
	h.Handle(msp.MethodTurnSteer, func(json.RawMessage) (any, error) {
		return nil, &msp.Error{Code: -32010, Message: "turn ended", Kind: "turnNotActive"}
	})
	c := msp.NewClient(conn)
	defer c.Close()
	err := c.Call(ctxT(t), msp.MethodTurnSteer, msp.TurnSteerParams{}, nil)
	var e *msp.Error
	if !errors.Is(err, msp.ErrTurnNotActive) || !errors.As(err, &e) || e.Code != -32010 {
		t.Fatalf("err = %#v", err)
	}
	if errors.Is(err, msp.ErrNotInitialized) {
		t.Fatal("kinds must not match each other")
	}
	if err := c.Call(ctxT(t), "nope/nothing", nil, nil); !errors.Is(err, msp.ErrMethodNotFound) {
		t.Fatalf("err = %v, want methodNotFound", err)
	}
}

func TestConcurrentCallsCorrelate(t *testing.T) {
	h, conn := msptest.New()
	h.Handle("echo", func(p json.RawMessage) (any, error) { return p, nil })
	c := msp.NewClient(conn)
	defer c.Close()
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out struct{ N int }
			if err := c.Call(ctxT(t), "echo", struct{ N int }{i}, &out); err != nil || out.N != i {
				t.Errorf("call %d = %d, %v", i, out.N, err)
			}
		}()
	}
	wg.Wait()
}

func TestNotificationsInOrderWithSlowConsumer(t *testing.T) {
	h, conn := msptest.New()
	h.Handle("ping", func(json.RawMessage) (any, error) { return "pong", nil })
	c := msp.NewClient(conn)
	defer c.Close()
	const n = 500
	for i := range n {
		if err := h.Notify("tick", map[string]int{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	// Nobody has read a notification yet; a call must still complete.
	var pong string
	if err := c.Call(ctxT(t), "ping", nil, &pong); err != nil || pong != "pong" {
		t.Fatalf("call blocked behind notifications: %q, %v", pong, err)
	}
	for i := range n {
		note := <-c.Notifications()
		v, err := msp.Decode[map[string]int](note)
		if err != nil || v["i"] != i {
			t.Fatalf("notification %d = %v, %v", i, v, err)
		}
	}
}

func TestServerRequestsAreAcknowledged(t *testing.T) {
	h, conn := msptest.New()
	c := msp.NewClient(conn)
	defer c.Close()
	if err := h.Request(7, msp.RequestApproval, map[string]string{"approvalId": "a"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range h.Received() {
			if m.ID != nil && string(*m.ID) == "7" && string(m.Result) == "{}" {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no receipt for approval/request; got %+v", h.Received())
}

func TestHandlerAnswersServerRequests(t *testing.T) {
	h, conn := msptest.New()
	got := make(chan string, 1)
	c := msp.NewClient(conn, msp.WithHandler(func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		got <- method
		return map[string]bool{"ok": true}, nil
	}))
	defer c.Close()
	_ = h.Request(1, msp.RequestUserInput, map[string]string{})
	select {
	case m := <-got:
		if m != msp.RequestUserInput {
			t.Fatalf("handler got %q", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler not called")
	}
}

func TestServerExitFailsPendingCalls(t *testing.T) {
	h, conn := msptest.New()
	h.Handle("slow", func(json.RawMessage) (any, error) {
		time.Sleep(time.Hour)
		return nil, nil
	})
	c := msp.NewClient(conn)
	errc := make(chan error, 1)
	go func() { errc <- c.Call(context.Background(), "slow", nil, nil) }()
	time.Sleep(20 * time.Millisecond)
	_ = h.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, msp.ErrClosed) {
			t.Fatalf("err = %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending call not failed when the server exited")
	}
	<-c.Done()
	if _, ok := <-c.Notifications(); ok {
		t.Fatal("notifications channel should close")
	}
	if err := c.Call(context.Background(), "x", nil, nil); !errors.Is(err, msp.ErrClosed) {
		t.Fatalf("call after close = %v", err)
	}
}

func TestContextCancelsCall(t *testing.T) {
	h, conn := msptest.New()
	block := make(chan struct{})
	defer close(block)
	h.Handle("slow", func(json.RawMessage) (any, error) { <-block; return nil, nil })
	c := msp.NewClient(conn)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.Call(ctx, "slow", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

type lineConn struct {
	io.Reader
	io.Writer
}

func (lineConn) Close() error { return nil }

func TestTooLargeLine(t *testing.T) {
	big := `{"jsonrpc":"2.0","method":"x","params":"` + strings.Repeat("a", 4096) + `"}` + "\n"
	c := msp.NewClient(lineConn{strings.NewReader(big), io.Discard}, msp.WithMaxMessageBytes(1024))
	<-c.Done()
	if !errors.Is(c.Err(), msp.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", c.Err())
	}
}

func TestReplayRecording(t *testing.T) {
	rec, err := msptest.Load("../testdata/msp-session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	turn := rec.Turn()
	var usage, done int
	for _, n := range turn {
		switch n.Method {
		case msp.NoteTokenUsage:
			u, err := msp.Decode[msp.TokenUsage](n)
			if err != nil || u.Usage.InputTokens == 0 || u.ModelID == nil {
				t.Fatalf("tokenUsage = %+v, %v", u, err)
			}
			usage++
		case msp.NoteTurnCompleted:
			c, err := msp.Decode[msp.TurnCompleted](n)
			if err != nil || c.Terminal != msp.TerminalCompleted {
				t.Fatalf("turn/completed = %+v, %v", c, err)
			}
			done++
		}
	}
	if usage != 5 || done != 1 {
		t.Fatalf("recording has %d usage and %d completions, want 5 and 1", usage, done)
	}
}

func TestCommandIDsAreSortedUUIDv7(t *testing.T) {
	prev := ""
	for range 10_000 {
		id := msp.NewCommandID()
		if len(id) != 36 || id[14] != '7' || !strings.ContainsRune("89ab", rune(id[19])) {
			t.Fatalf("not a UUIDv7: %s", id)
		}
		if id <= prev {
			t.Fatalf("ids not increasing: %s then %s", prev, id)
		}
		prev = id
	}
}

func FuzzClientDispatch(f *testing.F) {
	for _, s := range []string{
		`{"jsonrpc":"2.0","method":"session/tokenUsage","params":{"usage":{"inputTokens":1}}}`,
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
		`{"jsonrpc":"2.0","id":"x","error":{"code":1,"message":"m","data":{"kind":"k"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"approval/request","params":{}}`,
		`not json`, `{}`, `[]`, `{"id":null}`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		r := strings.NewReader(strings.ReplaceAll(line, "\n", " ") + "\n")
		c := msp.NewClient(lineConn{r, io.Discard})
		<-c.Done()
		for range c.Notifications() {
		}
	})
}

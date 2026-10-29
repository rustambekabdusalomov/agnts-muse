package credential

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAPIKey(t *testing.T) {
	c, err := APIKey("  LLM_abcdefghijklmnopqrstuvwxyz \n")
	if err != nil || c.Kind != KindAPIKey || c.APIKey != "LLM_abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("APIKey = %+v, %v", c, err)
	}
	for _, bad := range []string{"", "sk-123", "LLM_short", "LLM_abc def ghijklmnopqrstu"} {
		if _, err := APIKey(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("APIKey(%q) = %v", bad, err)
		}
	}
	env, files, err := c.Setup("")
	if err != nil || len(files) != 0 || len(env) != 1 || env[0] != "META_API_KEY=LLM_abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("Setup = %v %v %v", env, files, err)
	}
}

func TestNeverPrintsSecrets(t *testing.T) {
	k, _ := APIKey("LLM_supersecretvalue12345")
	l, _ := Login(Token{Access: "access-secret-xyz", Refresh: "refresh-secret-xyz"}, "dev@example.com")
	for _, c := range []Credential{k, l} {
		for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s", c, c, c, c)} {
			if strings.Contains(s, "secret") {
				t.Errorf("formatting leaked a secret: %s", s)
			}
		}
	}
}

func TestLoginAuthFileRoundTrip(t *testing.T) {
	exp := time.Unix(1_800_000_000, 0)
	c, err := Login(Token{Access: "acc-123456789", Refresh: "ref-123456789", ExpiresAt: exp}, "me")
	if err != nil {
		t.Fatal(err)
	}
	env, files, err := c.Setup("/home/agent/.config/muse/auth.json")
	if err != nil || len(env) != 1 || env[0] != "MUSE_AUTH_PATH=/home/agent/.config/muse/auth.json" || len(files) != 1 || files[0].Mode != 0o600 {
		t.Fatalf("Setup = %v %+v %v", env, files, err)
	}
	if !strings.Contains(string(files[0].Data), `"mechanism": "oauth"`) || !strings.Contains(string(files[0].Data), `"storage": "file"`) {
		t.Fatalf("auth file = %s", files[0].Data)
	}
	tok, err := ParseAuthFile(files[0].Data)
	if err != nil || tok.Access != "acc-123456789" || tok.Refresh != "ref-123456789" || !tok.ExpiresAt.Equal(exp) {
		t.Fatalf("ParseAuthFile = %+v, %v", tok, err)
	}
	if _, _, err := c.Setup(""); !errors.Is(err, ErrInvalid) {
		t.Fatal("a login without a path must fail")
	}
	if _, err := ParseAuthFile([]byte(`{"providers":{"meta":{"mechanism":"api_key"}}}`)); !errors.Is(err, ErrInvalid) {
		t.Fatal("a non-oauth file must fail")
	}
}

func TestRedactor(t *testing.T) {
	r := NewRedactor("LLM_supersecretvalue", "short", "")
	got := r.Redact("key=LLM_supersecretvalue and short stays")
	if got != "key=[redacted] and short stays" {
		t.Fatalf("Redact = %q", got)
	}
	if (Redactor{}).Redact("x") != "x" {
		t.Fatal("zero redactor changed text")
	}
}

func TestExpired(t *testing.T) {
	now := time.Now()
	if (&Token{}).Expired(now, time.Minute) {
		t.Fatal("no expiry means not expired")
	}
	if !(&Token{ExpiresAt: now.Add(30 * time.Second)}).Expired(now, time.Minute) {
		t.Fatal("within skew should count as expired")
	}
}

// fakeAuth is Meta's device endpoints: the person approves after polls
// pending polls, and the second poll asks the client to slow down.
func fakeAuth(t *testing.T, pending int, final string) (*httptest.Server, *atomic.Int32) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_id") != "cid" {
			http.Error(w, `{"error":"invalid_client"}`, 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oidc/device/authorization/":
			fmt.Fprint(w, `{"device_code":"dev-1","user_code":"ABCD-EFGH","verification_uri":"https://meta.ai/device",
				"verification_uri_complete":"https://meta.ai/device?code=ABCD-EFGH","expires_in":600,"interval":1}`)
		case "/oidc/device/token/":
			if r.Form.Get("grant_type") == "refresh_token" {
				fmt.Fprint(w, `{"access_token":"acc-2","refresh_token":"ref-2","expires_in":3600}`)
				return
			}
			n := polls.Add(1)
			switch {
			case int(n) == 2:
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":"slow_down"}`)
			case int(n) <= pending:
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":"authorization_pending"}`)
			default:
				if final != "" {
					w.WriteHeader(400)
					fmt.Fprintf(w, `{"error":%q}`, final)
					return
				}
				fmt.Fprint(w, `{"access_token":"acc-1","refresh_token":"ref-1","expires_in":3600}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &polls
}

func flow(srv *httptest.Server, slept *[]time.Duration) *DeviceFlow {
	now := time.Unix(1_800_000_000, 0)
	return &DeviceFlow{AuthURL: srv.URL, ClientID: "cid", HTTP: srv.Client(),
		Now: func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) error {
			*slept = append(*slept, d)
			now = now.Add(d)
			return nil
		}}
}

func TestDeviceFlow(t *testing.T) {
	srv, polls := fakeAuth(t, 3, "")
	var slept []time.Duration
	f := flow(srv, &slept)
	a, err := f.Start(context.Background())
	if err != nil || a.UserCode != "ABCD-EFGH" || a.DeviceCode != "dev-1" || a.Interval != time.Second {
		t.Fatalf("Start = %+v, %v", a, err)
	}
	tok, err := f.Wait(context.Background(), a)
	if err != nil || tok.Access != "acc-1" || tok.Refresh != "ref-1" || tok.ExpiresAt.IsZero() {
		t.Fatalf("Wait = %+v, %v", tok, err)
	}
	if polls.Load() != 4 {
		t.Fatalf("polls = %d, want 4", polls.Load())
	}
	// slow_down on the second poll adds 5s to every later wait.
	want := []time.Duration{time.Second, time.Second, 6 * time.Second, 6 * time.Second}
	if fmt.Sprint(slept) != fmt.Sprint(want) {
		t.Fatalf("waits = %v, want %v", slept, want)
	}
	r, err := f.Refresh(context.Background(), "ref-1")
	if err != nil || r.Access != "acc-2" {
		t.Fatalf("Refresh = %+v, %v", r, err)
	}
}

func TestDeviceFlowDeniedAndExpired(t *testing.T) {
	for _, tc := range []struct {
		final string
		want  error
	}{{"access_denied", ErrDenied}, {"expired_token", ErrExpired}} {
		srv, _ := fakeAuth(t, 0, tc.final)
		var slept []time.Duration
		f := flow(srv, &slept)
		a, err := f.Start(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Wait(context.Background(), a); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.final, err, tc.want)
		}
	}
	// The code's own lifetime ends the wait too.
	srv, _ := fakeAuth(t, 1<<30, "")
	var slept []time.Duration
	f := flow(srv, &slept)
	a, _ := f.Start(context.Background())
	if _, err := f.Wait(context.Background(), a); !errors.Is(err, ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
}

func TestDeviceFlowRejectsInsecurePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"device_code":"d","user_code":"u","verification_uri":"http://evil.example/device"}`)
	}))
	defer srv.Close()
	f := &DeviceFlow{AuthURL: srv.URL, HTTP: srv.Client()}
	if _, err := f.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "not https") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoginFileIsKeptAsIs(t *testing.T) {
	raw := []byte(`{"schema_version":1,"providers":{"meta":{"mechanism":"oauth","storage":"file","api_base_url":"https://api.meta.ai/v1","novel_field":"x","access_token":"tok-0123456789abcdefghij"}}}`)
	c, err := LoginFile(raw, "dev@example.com")
	if err != nil {
		t.Fatal(err)
	}
	_, files, err := c.Setup("/h/.config/muse/auth.json")
	if err != nil || string(files[0].Data) != string(raw) {
		t.Fatalf("file changed: %s, %v", files[0].Data, err)
	}
	if s := c.Secrets(); len(s) != 1 || s[0] != "tok-0123456789abcdefghij" {
		t.Fatalf("secrets = %v", s)
	}
	for _, bad := range []string{"", "{}", `{"providers":{}}`, "not json"} {
		if _, err := LoginFile([]byte(bad), ""); err == nil {
			t.Errorf("LoginFile(%q) accepted", bad)
		}
	}
}

func TestHosted(t *testing.T) {
	c, err := Hosted(" https://inference.baseten.co/v1/ ", "b10_abc.0123456789xyz", "")
	if err != nil {
		t.Fatal(err)
	}
	env, files, err := c.Setup("")
	if err != nil || len(files) != 0 || len(env) != 1 || env[0] != "META_API_KEY=b10_abc.0123456789xyz" {
		t.Fatalf("Setup = %v %v %v", env, files, err)
	}
	args := c.Args()
	if len(args) != 2 || args[0] != "-c" || args[1] != `endpoint_transport={"base_url":"https://inference.baseten.co/v1","auth":"bearer"}` {
		t.Fatalf("Args = %q", args)
	}
	if s := fmt.Sprintf("%v %#v", c, c); strings.Contains(s, "0123456789") {
		t.Fatalf("printing leaks the key: %s", s)
	}
	for _, bad := range [][2]string{
		{"http://inference.baseten.co/v1", "b10_abc.0123456789xyz"}, // not https
		{"https://user:pw@host/v1", "b10_abc.0123456789xyz"},        // credentials in the URL
		{"https://inference.baseten.co/v1", "short"},
		{"https://inference.baseten.co/v1", "two words 0123456789"},
	} {
		if _, err := Hosted(bad[0], bad[1], ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("Hosted(%q, %q) err = %v", bad[0], bad[1], err)
		}
	}
	if k, _ := APIKey("LLM_0123456789abcdefgh"); k.Args() != nil {
		t.Error("a Meta key got endpoint args")
	}
}

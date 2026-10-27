package credential

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Meta's sign-in service, as Muse Code's launcher uses it.
const (
	DefaultAuthURL  = "https://auth.meta.com"
	DefaultClientID = "1031625952748946"
	deviceGrant     = "urn:ietf:params:oauth:grant-type:device_code"
)

// Device flow errors (RFC 8628 §3.5).
var (
	ErrDenied  = errors.New("credential: sign-in was denied")
	ErrExpired = errors.New("credential: sign-in code expired before it was approved")
)

// DeviceFlow signs a person in with Meta's OAuth device flow (RFC 8628):
// Start returns a code to show them, Wait polls until they approve it in
// their browser. The zero value uses Meta's endpoints and Muse Code's
// client id.
type DeviceFlow struct {
	HTTP     *http.Client
	AuthURL  string // https://auth.meta.com
	ClientID string
	// Now and Sleep are replaced in tests.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// Authorization is a started sign-in.
type Authorization struct {
	DeviceCode string `json:"-"`
	UserCode   string `json:"user_code"`
	// VerificationURI is where the person goes; VerificationURIComplete
	// already carries the code, so they only confirm it.
	VerificationURI         string        `json:"verification_uri"`
	VerificationURIComplete string        `json:"verification_uri_complete,omitempty"`
	ExpiresAt               time.Time     `json:"expires_at"`
	Interval                time.Duration `json:"-"`
}

func (f *DeviceFlow) defaults() {
	if f.HTTP == nil {
		f.HTTP = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // like the launcher: never follow redirects with a device code
		}}
	}
	if f.AuthURL == "" {
		f.AuthURL = DefaultAuthURL
	}
	if f.ClientID == "" {
		f.ClientID = DefaultClientID
	}
	if f.Now == nil {
		f.Now = time.Now
	}
	if f.Sleep == nil {
		f.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// Start begins a sign-in.
func (f *DeviceFlow) Start(ctx context.Context) (Authorization, error) {
	f.defaults()
	var r struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	status, err := f.post(ctx, "/oidc/device/authorization/", url.Values{"client_id": {f.ClientID}}, &r)
	if err != nil {
		return Authorization{}, err
	}
	if status/100 != 2 || r.DeviceCode == "" || r.UserCode == "" {
		return Authorization{}, fmt.Errorf("credential: sign-in could not start (HTTP %d)", status)
	}
	for _, u := range []string{r.VerificationURI, r.VerificationURIComplete} {
		if u != "" && !strings.HasPrefix(u, "https://") {
			return Authorization{}, fmt.Errorf("credential: sign-in page %q is not https", u)
		}
	}
	// Bounds as the launcher applies them: 1 to 30 minutes, 5s default poll.
	life := min(max(r.ExpiresIn, 60), 1800)
	if r.ExpiresIn == 0 {
		life = 900
	}
	interval := time.Duration(r.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return Authorization{DeviceCode: r.DeviceCode, UserCode: r.UserCode, VerificationURI: r.VerificationURI,
		VerificationURIComplete: r.VerificationURIComplete, ExpiresAt: f.Now().Add(time.Duration(life) * time.Second), Interval: interval}, nil
}

// Wait polls until the person approves the sign-in, denies it, or it
// expires. It returns the token.
func (f *DeviceFlow) Wait(ctx context.Context, a Authorization) (Token, error) {
	f.defaults()
	interval := a.Interval
	for {
		if !f.Now().Before(a.ExpiresAt) {
			return Token{}, ErrExpired
		}
		if err := f.Sleep(ctx, interval); err != nil {
			return Token{}, err
		}
		t, pending, err := f.poll(ctx, url.Values{"grant_type": {deviceGrant}, "device_code": {a.DeviceCode}, "client_id": {f.ClientID}})
		switch {
		case err != nil:
			return Token{}, err
		case pending == "slow_down":
			interval += 5 * time.Second
		case pending == "":
			return t, nil
		}
	}
}

// Refresh exchanges a refresh token for a new token.
func (f *DeviceFlow) Refresh(ctx context.Context, refresh string) (Token, error) {
	f.defaults()
	t, pending, err := f.poll(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {f.ClientID}})
	if err == nil && pending != "" {
		err = fmt.Errorf("credential: refresh: %s", pending)
	}
	return t, err
}

// poll calls the token endpoint. pending is "authorization_pending" or
// "slow_down" while the person has not decided yet.
func (f *DeviceFlow) poll(ctx context.Context, form url.Values) (t Token, pending string, err error) {
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		Error        string `json:"error"`
	}
	status, err := f.post(ctx, "/oidc/device/token/", form, &r)
	if err != nil {
		return Token{}, "", err
	}
	switch {
	case status/100 == 2 && r.AccessToken != "":
		t = Token{Access: r.AccessToken, Refresh: r.RefreshToken, Scope: r.Scope}
		if r.ExpiresIn > 0 {
			t.ExpiresAt = f.Now().Add(time.Duration(r.ExpiresIn) * time.Second)
		}
		return t, "", nil
	case r.Error == "authorization_pending" || r.Error == "slow_down":
		return Token{}, r.Error, nil
	case r.Error == "access_denied":
		return Token{}, "", ErrDenied
	case r.Error == "expired_token":
		return Token{}, "", ErrExpired
	}
	return Token{}, "", fmt.Errorf("credential: sign-in failed (HTTP %d %s)", status, r.Error)
}

func (f *DeviceFlow) post(ctx context.Context, path string, form url.Values, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(f.AuthURL, "/")+path, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "agnts-muse")
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("credential: sign-in service: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if len(body) > 0 {
		_ = json.Unmarshal(body, out) // an error page leaves out empty; the status says why
	}
	return resp.StatusCode, nil
}

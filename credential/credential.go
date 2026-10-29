// Package credential holds the ways a Muse Code process authenticates to
// Meta, and gets them into the process without leaking them.
//
// Muse accepts two credentials. An API key (Meta Model API, "LLM_…") goes
// in META_API_KEY, which always wins over a login. A hosted key ([Hosted])
// is the same, for another host that serves Muse models on Muse's API
// (Baseten serves Muse Spark Open). A login is an OAuth
// token from Meta's device flow (what `muse login` does); Muse reads it
// from its credential file, whose location MUSE_AUTH_PATH overrides. This
// package can run that device flow itself ([DeviceFlow]), so a web app can
// show the code to its user and keep the token, instead of asking them to
// paste anything.
package credential

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Kind is how a credential authenticates.
type Kind string

// Kinds.
const (
	KindAPIKey Kind = "api_key"
	KindLogin  Kind = "login"
)

// APIKeyPrefix starts every Meta Model API key.
const APIKeyPrefix = "LLM_"

// Environment variables Muse Code reads.
const (
	EnvAPIKey   = "META_API_KEY" //nolint:gosec // a variable name, not a credential
	EnvAuthPath = "MUSE_AUTH_PATH"
)

// DefaultAPIBase is the Meta Model API.
const DefaultAPIBase = "https://api.meta.ai/v1"

// ErrInvalid reports a credential that cannot be used.
var ErrInvalid = errors.New("credential: invalid")

// Credential is one way to reach Muse. Its String and GoString never print
// the secret.
type Credential struct {
	Kind Kind
	// APIKey is set for KindAPIKey.
	APIKey string
	// Token is set for KindLogin made from a token (Login).
	Token *Token
	// File is set for KindLogin taken as Muse Code saved it (LoginFile):
	// its credential file, passed to Muse unchanged.
	File []byte
	// Label names the account for people ("rustam@example.com").
	Label string
	// BaseURL is set for a Hosted key: the host Muse calls instead of
	// DefaultAPIBase.
	BaseURL string
}

// Token is an OAuth token from Meta's device flow.
type Token struct {
	Access    string    `json:"access_token"`
	Refresh   string    `json:"refresh_token,omitempty"`
	ExpiresAt time.Time `json:"-"`
	Scope     string    `json:"scope,omitempty"`
}

// Expired reports whether t expires within skew of now.
func (t *Token) Expired(now time.Time, skew time.Duration) bool {
	return !t.ExpiresAt.IsZero() && !now.Add(skew).Before(t.ExpiresAt)
}

// APIKey returns an API key credential. It checks the key's shape only.
func APIKey(key string) (Credential, error) {
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, APIKeyPrefix) || len(key) < len(APIKeyPrefix)+16 || strings.ContainsAny(key, " \t\r\n\"'") {
		return Credential{}, fmt.Errorf("%w: a Meta Model API key starts with %s", ErrInvalid, APIKeyPrefix)
	}
	return Credential{Kind: KindAPIKey, APIKey: key}, nil
}

// Hosted returns an API key for another host that serves Muse models on
// Muse's API, such as Baseten's OpenAI-compatible endpoint
// ("https://inference.baseten.co/v1"). Its keys have their own shape, so
// only that it is one plain token is checked.
func Hosted(baseURL, key, label string) (Credential, error) {
	key = strings.TrimSpace(key)
	u, err := url.Parse(strings.TrimSpace(baseURL))
	switch {
	case err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil:
		return Credential{}, fmt.Errorf("%w: a host's base URL must be https://host/path", ErrInvalid)
	case len(key) < 16 || strings.ContainsAny(key, " \t\r\n\"'"):
		return Credential{}, fmt.Errorf("%w: a host's API key is one token of at least 16 characters", ErrInvalid)
	}
	return Credential{Kind: KindAPIKey, APIKey: key, Label: label, BaseURL: strings.TrimSuffix(u.String(), "/")}, nil
}

// Login returns a login credential from a token.
func Login(t Token, label string) (Credential, error) {
	if t.Access == "" {
		return Credential{}, fmt.Errorf("%w: login has no access token", ErrInvalid)
	}
	return Credential{Kind: KindLogin, Token: &t, Label: label}, nil
}

// LoginFile is a login as Muse Code itself saved it after `muse login`:
// its credential file, kept byte for byte, so nothing about its format is
// assumed. It must be a JSON object with a Meta provider entry.
func LoginFile(data []byte, label string) (Credential, error) {
	var f struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(data, &f); err != nil || len(f.Providers["meta"]) == 0 {
		return Credential{}, fmt.Errorf("%w: not a Muse Code credential file with a Meta login", ErrInvalid)
	}
	return Credential{Kind: KindLogin, File: append([]byte(nil), data...), Label: label}, nil
}

// fileSecrets are the long string values of a credential file's Meta
// entry (tokens), for redaction; URLs and short labels are left out.
func fileSecrets(data []byte) []string {
	var f struct {
		Providers map[string]map[string]any `json:"providers"`
	}
	if json.Unmarshal(data, &f) != nil {
		return nil
	}
	var out []string
	for _, v := range f.Providers["meta"] {
		if s, ok := v.(string); ok && len(s) >= 20 && !strings.HasPrefix(s, "http") {
			out = append(out, s)
		}
	}
	return out
}

// Secrets returns every secret string in c, for redaction.
func (c Credential) Secrets() []string {
	var s []string
	s = append(s, fileSecrets(c.File)...)
	if c.APIKey != "" {
		s = append(s, c.APIKey)
	}
	if c.Token != nil {
		for _, v := range []string{c.Token.Access, c.Token.Refresh} {
			if v != "" {
				s = append(s, v)
			}
		}
	}
	return s
}

func (c Credential) String() string {
	if c.BaseURL != "" {
		return fmt.Sprintf("muse %s at %s", c.Kind, c.BaseURL)
	}
	if c.Label != "" {
		return fmt.Sprintf("muse %s (%s)", c.Kind, c.Label)
	}
	return fmt.Sprintf("muse %s", c.Kind)
}

// GoString keeps %#v from printing secrets.
func (c Credential) GoString() string { return "credential.Credential{" + c.String() + "}" }

// File is a file a process needs before it starts.
type File struct {
	Path string
	Data []byte
	Mode uint32
}

// Setup is how a process gets a credential: environment variables and
// files. authPath is where a login's credential file goes (ignored for an
// API key); the caller owns that path and removes it afterwards.
func (c Credential) Setup(authPath string) (env []string, files []File, err error) {
	switch c.Kind {
	case KindAPIKey:
		return []string{EnvAPIKey + "=" + c.APIKey}, nil, nil
	case KindLogin:
		if authPath == "" {
			return nil, nil, fmt.Errorf("%w: a login needs a path for its credential file", ErrInvalid)
		}
		data, err := c.AuthFile()
		if err != nil {
			return nil, nil, err
		}
		return []string{EnvAuthPath + "=" + authPath}, []File{{Path: authPath, Data: data, Mode: 0o600}}, nil
	}
	return nil, nil, fmt.Errorf("%w: kind %q", ErrInvalid, c.Kind)
}

// Args are the `muse serve` arguments a credential needs. A Hosted key
// needs the host pinned in Muse's endpoint_transport setting with bearer
// auth: Muse withholds the key from any other base URL (a --base-url flag
// alone is not enough), so a key cannot leak to a host by accident.
func (c Credential) Args() []string {
	if c.BaseURL == "" {
		return nil
	}
	v, _ := json.Marshal(struct {
		BaseURL string `json:"base_url"`
		Auth    string `json:"auth"`
	}{c.BaseURL, "bearer"})
	return []string{"-c", "endpoint_transport=" + string(v)}
}

// authFile is Muse Code's credential file (schema 1), as `muse login`
// writes it with file storage.
type authFile struct {
	SchemaVersion int                     `json:"schema_version"`
	Providers     map[string]authProvider `json:"providers"`
}

type authProvider struct {
	Mechanism   string `json:"mechanism"`
	Storage     string `json:"storage"`
	ObtainedVia string `json:"obtained_via"`
	APIBaseURL  string `json:"api_base_url"`
	AccessToken string `json:"access_token"`
	Refresh     string `json:"refresh_token,omitempty"`
	ExpiresAt   int64  `json:"expires_at,omitempty"`
}

// AuthFile renders a login as Muse Code's credential file (the file
// itself for a LoginFile).
func (c Credential) AuthFile() ([]byte, error) {
	if c.Kind == KindLogin && c.File != nil {
		return append([]byte(nil), c.File...), nil
	}
	if c.Kind != KindLogin || c.Token == nil {
		return nil, fmt.Errorf("%w: only a login has a credential file", ErrInvalid)
	}
	p := authProvider{Mechanism: "oauth", Storage: "file", ObtainedVia: "device_code", APIBaseURL: DefaultAPIBase,
		AccessToken: c.Token.Access, Refresh: c.Token.Refresh}
	if !c.Token.ExpiresAt.IsZero() {
		p.ExpiresAt = c.Token.ExpiresAt.Unix()
	}
	return json.MarshalIndent(authFile{SchemaVersion: 1, Providers: map[string]authProvider{"meta": p}}, "", "  ")
}

// ParseAuthFile reads a credential file back, such as the one Muse
// rewrote after refreshing its token during a run.
func ParseAuthFile(data []byte) (Token, error) {
	var f authFile
	if err := json.Unmarshal(data, &f); err != nil {
		return Token{}, fmt.Errorf("%w: credential file: %w", ErrInvalid, err)
	}
	p, ok := f.Providers["meta"]
	if !ok || p.Mechanism != "oauth" || p.AccessToken == "" {
		return Token{}, fmt.Errorf("%w: credential file has no Meta login", ErrInvalid)
	}
	t := Token{Access: p.AccessToken, Refresh: p.Refresh}
	if p.ExpiresAt > 0 {
		t.ExpiresAt = time.Unix(p.ExpiresAt, 0)
	}
	return t, nil
}

// Redactor replaces secrets in text with "[redacted]". The zero value
// redacts nothing.
type Redactor struct{ r *strings.Replacer }

// NewRedactor redacts each non-empty secret of at least 8 characters
// (shorter strings would mangle ordinary text).
func NewRedactor(secrets ...string) Redactor {
	var pairs []string
	for _, s := range secrets {
		if len(s) >= 8 {
			pairs = append(pairs, s, "[redacted]")
		}
	}
	if len(pairs) == 0 {
		return Redactor{}
	}
	return Redactor{strings.NewReplacer(pairs...)}
}

// Redact returns s without secrets.
func (r Redactor) Redact(s string) string {
	if r.r == nil {
		return s
	}
	return r.r.Replace(s)
}

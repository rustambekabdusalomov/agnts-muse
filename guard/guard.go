// Package guard decides Muse's tool approvals.
//
// With approval mode promptUnmatched, Muse asks its client before running a
// tool call its own policy does not already allow (MSP approval/requested).
// A Policy answers in the client's place, so an unattended run gets the
// same rules a person would apply: no pushing, no reading secrets, no
// piping the internet into a shell. A denial goes back to the model as
// feedback, so it can choose another way instead of failing.
package guard

import (
	"path"
	"regexp"
	"strings"
)

// Request is a tool call waiting for approval.
type Request struct {
	Tool    string     // bash, read_file, edit_file, web_fetch, …
	Kind    string     // shell, file, network, … (MSP approval subject kind)
	Command string     // the shell command, for shell calls
	Argv    [][]string // each stage's argv, when Muse parsed the command
	Path    string     // the file, for file calls
	Host    string     // the host, for network calls
	Args    string     // the model's raw argument JSON
}

// Verdict is a policy's answer.
type Verdict struct {
	Allow bool
	// Reason is shown to the model when the call is denied, and logged.
	Reason string
	// Rule names the rule that decided, for audit logs.
	Rule string
}

// Policy decides approvals.
type Policy interface {
	Decide(Request) Verdict
}

// Func adapts a function to Policy.
type Func func(Request) Verdict

// Decide implements Policy.
func (f Func) Decide(r Request) Verdict { return f(r) }

// Rule denies a matching tool call.
type Rule struct {
	Name string
	// Command matches a shell command (the whole line and every stage).
	Command *regexp.Regexp
	// Path matches a file path with path.Match, against the full path and
	// its base name.
	Path string
	// Host matches a network host exactly or as a suffix (".example.com").
	Host   string
	Reason string
}

func (r Rule) matches(req Request) bool {
	if r.Command != nil {
		if req.Command != "" && r.Command.MatchString(req.Command) {
			return true
		}
		for _, st := range req.Argv {
			if r.Command.MatchString(strings.Join(st, " ")) {
				return true
			}
		}
	}
	if r.Path != "" && req.Path != "" {
		if ok, _ := path.Match(r.Path, req.Path); ok {
			return true
		}
		if ok, _ := path.Match(r.Path, path.Base(req.Path)); ok {
			return true
		}
	}
	if r.Host != "" && req.Host != "" {
		h := strings.ToLower(req.Host)
		if h == r.Host || strings.HasPrefix(r.Host, ".") && strings.HasSuffix(h, r.Host) {
			return true
		}
	}
	return false
}

// Rules denies a call when any rule matches and allows everything else.
type Rules []Rule

// Decide implements Policy.
func (rs Rules) Decide(req Request) Verdict {
	for _, r := range rs {
		if r.matches(req) {
			return Verdict{Allow: false, Reason: r.Reason, Rule: r.Name}
		}
	}
	return Verdict{Allow: true, Rule: "default-allow"}
}

// Chain asks each policy in turn; the first denial wins.
type Chain []Policy

// Decide implements Policy.
func (c Chain) Decide(req Request) Verdict {
	v := Verdict{Allow: true, Rule: "default-allow"}
	for _, p := range c {
		if v = p.Decide(req); !v.Allow {
			return v
		}
	}
	return v
}

// ReadOnly allows only calls that cannot change anything: reading files
// and a few inspection commands. Quotes and reviews run under it.
var ReadOnly = Func(func(r Request) Verdict {
	switch r.Kind {
	case "file":
		if strings.Contains(r.Tool, "read") || strings.Contains(r.Tool, "list") || strings.Contains(r.Tool, "search") || strings.Contains(r.Tool, "grep") {
			return Verdict{Allow: true, Rule: "read-only"}
		}
	case "shell":
		if readOnlyCmd.MatchString(strings.TrimSpace(r.Command)) && !strings.ContainsAny(r.Command, ">;&|`$") {
			return Verdict{Allow: true, Rule: "read-only"}
		}
	}
	return Verdict{Allow: false, Rule: "read-only", Reason: "this run is read-only: inspect the code, do not change it or run commands that do"}
})

var readOnlyCmd = regexp.MustCompile(`^(ls|cat|head|tail|wc|find|grep|rg|git (log|show|diff|status|blame))(\s|$)`)

// Defaults are the rules agnts applies to every coding run. They mirror
// agnts-guard's hooks for Claude Code: the server pushes, never the agent;
// secrets stay unread; nothing from the internet is piped into a shell.
func Defaults() Rules {
	return Rules{
		{Name: "no-push", Command: regexp.MustCompile(`\bgit\s+(push|remote\s+(add|set-url))\b`),
			Reason: "do not push or change remotes: agnts pushes your branch after a person approves it"},
		{Name: "no-force-history", Command: regexp.MustCompile(`\bgit\s+(reset\s+--hard|rebase|filter-branch|reflog\s+expire)\b`),
			Reason: "do not rewrite history; make a new commit instead"},
		{Name: "no-pipe-to-shell", Command: regexp.MustCompile(`\b(curl|wget)\b[^|]*\|\s*(sudo\s+)?(sh|bash|zsh|python3?)\b`),
			Reason: "do not pipe downloads into a shell; add the dependency through the package manager"},
		{Name: "no-rm-root", Command: regexp.MustCompile(`\brm\s+(-[a-zA-Z]*r[a-zA-Z]*f?|-[a-zA-Z]*f[a-zA-Z]*r)\s+(/|~|\$HOME|\.\.)(\s|$)`),
			Reason: "do not delete outside the repository"},
		{Name: "no-sudo", Command: regexp.MustCompile(`(^|[;&|]\s*)sudo\b`),
			Reason: "there is no sudo here; tools are installed for you"},
		{Name: "no-skip-hooks", Command: regexp.MustCompile(`--no-verify\b`),
			Reason: "do not skip checks; fix what they report"},
		{Name: "no-env-files", Path: ".env*", Reason: "do not read or write .env files: they hold secrets"},
		{Name: "no-keys", Path: "*.pem", Reason: "do not touch private keys"},
		{Name: "no-ssh", Path: "*/.ssh/*", Reason: "do not touch SSH keys"},
		{Name: "no-cat-env", Command: regexp.MustCompile(`\b(cat|less|head|tail|more|source|\.)\s+\S*\.env\b|\bprintenv\b|^\s*env\s*$`),
			Reason: "do not print environment files or variables: they hold secrets"},
	}
}

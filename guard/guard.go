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
	// Ask leaves the call to a person (Allow is false until they allow
	// it). The run asks one when it can, and denies the call when it
	// cannot.
	Ask bool
	// Reason is shown to the model when the call is denied, to the person
	// when they are asked, and logged.
	Reason string
	// Rule names the rule that decided, for audit logs.
	Rule string
}

// denies reports whether v refuses the call outright.
func (v Verdict) denies() bool { return !v.Allow && !v.Ask }

// Policy decides approvals.
type Policy interface {
	Decide(Request) Verdict
}

// Func adapts a function to Policy.
type Func func(Request) Verdict

// Decide implements Policy.
func (f Func) Decide(r Request) Verdict { return f(r) }

// Rule denies a matching tool call, or asks a person about it.
type Rule struct {
	Name string
	// Ask asks a person about a match instead of denying it.
	Ask bool
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

// Rules denies a call when a deny rule matches, asks a person when only an
// ask rule does, and allows everything else.
type Rules []Rule

// Decide implements Policy.
func (rs Rules) Decide(req Request) Verdict {
	var ask *Rule
	for i, r := range rs {
		if !r.matches(req) {
			continue
		}
		if !r.Ask {
			return Verdict{Allow: false, Reason: r.Reason, Rule: r.Name}
		}
		if ask == nil {
			ask = &rs[i]
		}
	}
	if ask != nil {
		return Verdict{Ask: true, Reason: ask.Reason, Rule: ask.Name}
	}
	return Verdict{Allow: true, Rule: "default-allow"}
}

// Chain asks each policy in turn: the first denial wins, then the first
// question for a person, else the call is allowed.
type Chain []Policy

// Decide implements Policy.
func (c Chain) Decide(req Request) Verdict {
	v := Verdict{Allow: true, Rule: "default-allow"}
	var ask *Verdict
	for _, p := range c {
		x := p.Decide(req)
		if x.denies() {
			return x
		}
		if x.Ask && ask == nil {
			ask = &x
		}
		v = x
	}
	if ask != nil {
		return *ask
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
		if readOnlyShell(r) {
			return Verdict{Allow: true, Rule: "read-only"}
		}
	}
	return Verdict{Allow: false, Rule: "read-only", Reason: "this run is read-only: inspect the code with read-only commands (ls, cat, grep, find, git log …, " +
		"pipes and ; are fine); do not change files or redirect output into them"}
})

// ReadOnlyExcept is ReadOnly that may also write inside dirs: a run that
// must not change the code but hands its result back in a file (agnts's
// analysis writes /agnts/out/analysis.json). File tools may write there;
// shell commands may create the directory, copy or tee into it and
// redirect into it, as long as every other stage only reads.
func ReadOnlyExcept(dirs ...string) Policy {
	inside := func(p string) bool {
		p = path.Clean(strings.Trim(p, `"'`))
		for _, d := range dirs {
			d = path.Clean(d)
			if p == d || strings.HasPrefix(p, d+"/") {
				return true
			}
		}
		return false
	}
	return Func(func(r Request) Verdict {
		if v := ReadOnly.Decide(r); v.Allow {
			return v
		}
		allow := Verdict{Allow: true, Rule: "read-only-except"}
		switch r.Kind {
		case "file":
			if r.Path != "" && inside(r.Path) {
				return allow
			}
		case "shell":
			if writesOnlyInside(r, inside) {
				return allow
			}
		}
		return Verdict{Allow: false, Rule: "read-only", Reason: "this run is read-only except " + strings.Join(dirs, ", ") +
			": inspect the code with read-only commands and write only your result there (e.g. cat > " + path.Join(dirs[0], "result.json") + " <<'EOF' … EOF)"}
	})
}

// writesOnlyInside reports whether a shell command reads, and writes only
// into allowed dirs: redirects there or to /dev/null, mkdir/cp/tee/touch
// whose targets are there. Interpreters (python, node, …) are refused:
// what they write cannot be checked.
func writesOnlyInside(r Request, inside func(string) bool) bool {
	sc, ok := parseShell(r.Command)
	if !ok || len(sc.stages) == 0 {
		return false
	}
	for _, t := range sc.outputs {
		if t != "/dev/null" && !strings.HasPrefix(t, "&") && !inside(t) {
			return false
		}
	}
	for _, f := range sc.stages {
		switch path.Base(f[0]) {
		case "mkdir", "touch", "tee":
			for _, a := range f[1:] {
				if !strings.HasPrefix(a, "-") && !inside(a) {
					return false
				}
			}
		case "cp", "mv":
			if len(f) < 3 || !inside(f[len(f)-1]) {
				return false
			}
		default:
			if !readOnlyStage(f) {
				return false
			}
		}
	}
	return true
}

// readOnlyShell reports whether every stage of a shell command only reads:
// pipelines and sequences (|, ;, &&, ||) of inspection commands, with
// output redirected nowhere but /dev/null. Refusing `ls -R | head` would
// only make the model spend calls working around the rule.
func readOnlyShell(r Request) bool {
	stages := r.Argv
	if cmd := strings.TrimSpace(r.Command); cmd != "" {
		sc, ok := parseShell(cmd)
		if !ok {
			return false
		}
		for _, t := range sc.outputs {
			if t != "/dev/null" && !strings.HasPrefix(t, "&") {
				return false
			}
		}
		stages = sc.stages
	}
	if len(stages) == 0 {
		return false
	}
	for _, st := range stages {
		if !readOnlyStage(st) {
			return false
		}
	}
	return true
}

var (
	// readOnlyBins only read (find and sed are checked for their writing
	// flags below).
	readOnlyBins = map[string]bool{"ls": true, "cat": true, "head": true, "tail": true, "wc": true, "find": true, "grep": true,
		"egrep": true, "rg": true, "tree": true, "file": true, "stat": true, "echo": true, "printf": true, "pwd": true, "sort": true,
		"uniq": true, "cut": true, "tr": true, "jq": true, "sed": true, "awk": true, "basename": true, "dirname": true, "realpath": true,
		"du": true, "diff": true, "true": true, "test": true, "[": true, "nl": true, "column": true, "fd": true}
	readOnlyGit = map[string]bool{"log": true, "show": true, "diff": true, "status": true, "blame": true, "ls-files": true,
		"grep": true, "branch": true, "rev-parse": true, "shortlog": true, "describe": true, "ls-tree": true, "cat-file": true}
)

func readOnlyStage(argv []string) bool {
	for len(argv) > 0 && strings.Contains(argv[0], "=") && !strings.HasPrefix(argv[0], "-") {
		argv = argv[1:] // VAR=value prefixes
	}
	if len(argv) == 0 {
		return true
	}
	name := path.Base(argv[0])
	switch name {
	case "git":
		sub := ""
		for _, a := range argv[1:] {
			if !strings.HasPrefix(a, "-") {
				sub = a
				break
			}
		}
		return readOnlyGit[sub]
	case "find":
		for _, a := range argv[1:] {
			switch a {
			case "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprintf", "-fls":
				return false
			}
		}
	case "sed":
		for _, a := range argv[1:] {
			if a == "-i" || strings.HasPrefix(a, "-i") || strings.HasPrefix(a, "--in-place") {
				return false
			}
		}
	case "awk":
		for _, a := range argv[1:] {
			if strings.Contains(a, "system(") || strings.Contains(a, "print >") || strings.Contains(a, "printf >") {
				return false
			}
		}
	}
	return readOnlyBins[name]
}

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

// AskBeforeInstalls asks a person before the agent adds a dependency
// (a package manager's add or install of a named package). Installing what
// the project already names (npm ci, npm install with no package, pip
// install -r or -e .) goes ahead. For runs a person is following, such as
// a chat.
func AskBeforeInstalls() Rules {
	return Rules{
		{Name: "ask-new-dependency", Ask: true, Reason: "it adds a dependency to the project",
			Command: regexp.MustCompile(`(^|[;&|]\s*)(` +
				`(npm|pnpm)\s+(install|i|add)(\s+-\S+)*\s+[^-\s]` +
				`|yarn\s+(global\s+)?add\s` +
				`|(pip3?|uv\s+pip)\s+install(\s+-[^re\s]\S*)*\s+[^-\s.]` +
				`|(uv|poetry|cargo|bundle|pnpm|bun|deno)\s+add\s` +
				`|go\s+get\s+\S` +
				`|gem\s+install\s` +
				`|composer\s+require\s` +
				`)`)},
	}
}

package guard

import "strings"

// shellCmd is a shell command line split the way a POSIX shell would
// split it, as far as the guard needs: the argv of each stage of its
// pipelines and sequences, and where it redirects output. Quotes and
// escapes are honoured, so `grep "a\|b"` is one stage with a pattern, not
// a pipe; here-document bodies are data and skipped.
type shellCmd struct {
	stages  [][]string
	outputs []string // output redirection targets ("&1" for fd copies)
}

// parseShell splits cmd. ok is false for what the guard cannot reason
// about: command substitution, process substitution, unclosed quotes.
func parseShell(cmd string) (sc shellCmd, ok bool) {
	var (
		stage    []string
		word     strings.Builder
		inWord   bool
		heredocs []string // delimiters waiting for the end of the line
	)
	endWord := func() {
		if inWord {
			stage = append(stage, word.String())
			word.Reset()
			inWord = false
		}
	}
	endStage := func() {
		endWord()
		if len(stage) > 0 {
			sc.stages = append(sc.stages, stage)
		}
		stage = nil
	}
	// readTarget reads the word after a redirection operator.
	readTarget := func(s string, i int) (string, int) {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		var t strings.Builder
		q := byte(0)
		for ; i < len(s); i++ {
			c := s[i]
			switch {
			case q != 0 && c == q:
				q = 0
			case q != 0:
				t.WriteByte(c)
			case c == '\'' || c == '"':
				q = c
			case strings.IndexByte(" \t\n;|&<>", c) >= 0:
				return t.String(), i
			default:
				t.WriteByte(c)
			}
		}
		return t.String(), i
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case c == '\\' && i+1 < len(cmd):
			i++
			if cmd[i] != '\n' {
				word.WriteByte(cmd[i])
				inWord = true
			}
		case c == '\'':
			j := strings.IndexByte(cmd[i+1:], '\'')
			if j < 0 {
				return sc, false
			}
			word.WriteString(cmd[i+1 : i+1+j])
			inWord = true
			i += j + 1
		case c == '"':
			i++
			for ; i < len(cmd) && cmd[i] != '"'; i++ {
				if cmd[i] == '\\' && i+1 < len(cmd) && strings.IndexByte(`"\$`+"`", cmd[i+1]) >= 0 {
					i++
				} else if cmd[i] == '`' || cmd[i] == '$' && i+1 < len(cmd) && cmd[i+1] == '(' {
					return sc, false
				}
				word.WriteByte(cmd[i])
			}
			if i >= len(cmd) {
				return sc, false
			}
			inWord = true
		case c == '`', c == '$' && i+1 < len(cmd) && cmd[i+1] == '(', c == '<' && i+1 < len(cmd) && cmd[i+1] == '(':
			return sc, false
		case c == '<' && i+1 < len(cmd) && cmd[i+1] == '<':
			endWord()
			i += 2
			if i < len(cmd) && cmd[i] == '-' {
				i++
			}
			var d string
			d, i = readTarget(cmd, i)
			i--
			heredocs = append(heredocs, d)
		case c == '>' || (c >= '0' && c <= '9' && !inWord && i+1 < len(cmd) && cmd[i+1] == '>'):
			endWord()
			if c != '>' {
				i++
			}
			i++
			if i < len(cmd) && cmd[i] == '>' {
				i++
			}
			if i < len(cmd) && cmd[i] == '&' {
				var t string
				t, i = readTarget(cmd, i+1)
				sc.outputs = append(sc.outputs, "&"+t)
			} else {
				var t string
				t, i = readTarget(cmd, i)
				sc.outputs = append(sc.outputs, t)
			}
			i--
		case c == '<':
			endWord()
			_, j := readTarget(cmd, i+1) // input redirection reads a file
			i = j - 1
		case c == '|' || c == ';' || c == '&':
			endStage()
			if i+1 < len(cmd) && (cmd[i+1] == '|' || cmd[i+1] == '&') && cmd[i+1] == c {
				i++
			}
		case c == '\n':
			endStage()
			// Skip here-document bodies that start after this line.
			for _, d := range heredocs {
				for i+1 < len(cmd) {
					end := strings.IndexByte(cmd[i+1:], '\n')
					line := cmd[i+1:]
					if end >= 0 {
						line = cmd[i+1 : i+1+end]
					}
					if end < 0 {
						i = len(cmd)
					} else {
						i += end + 1
					}
					if strings.TrimSpace(line) == d {
						break
					}
				}
			}
			heredocs = nil
		case c == ' ' || c == '\t':
			endWord()
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	endStage()
	return sc, true
}

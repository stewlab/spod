// SPDX-License-Identifier: Apache-2.0

// Containerfile parsing for spod build: the instruction grammar (parser
// directives, comments, line continuations, JSON and shell forms, flags) and
// the shell-style word expansion applied to instruction arguments.

package engine

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
)

// cfInstruction is one logical Containerfile instruction.
type cfInstruction struct {
	Line int
	// Cmd is the lowercase instruction keyword.
	Cmd string
	// Flags holds leading --name=value options, keyed by name.
	Flags map[string]string
	// JSON is the exec-form argument list when JSON is true.
	JSON   []string
	IsJSON bool
	// Rest is the argument text after flags, unexpanded.
	Rest string
	// Original is the instruction as written, continuations joined.
	Original string
}

// cfStage is a FROM instruction and the instructions that follow it.
type cfStage struct {
	From         cfInstruction
	Name         string
	Instructions []cfInstruction
}

// containerfile is a parsed Containerfile: the ARGs before the first FROM
// and the build stages.
type containerfile struct {
	GlobalArgs []cfInstruction
	Stages     []cfStage
	// Escape is the escape character from the escape parser directive.
	Escape rune
}

var (
	directiveRE = regexp.MustCompile(`^#\s*([a-zA-Z][a-zA-Z0-9]*)\s*=\s*(.+?)\s*$`)
	heredocRE   = regexp.MustCompile(`<<-?\s*["']?[A-Za-z_]`)
)

// instructionFlags lists the options each instruction accepts.
var instructionFlags = map[string][]string{
	"from":        {"platform"},
	"run":         {"network"},
	"copy":        {"from", "chown", "chmod", "link"},
	"add":         {"chown", "chmod", "link"},
	"healthcheck": {"interval", "timeout", "start-period", "start-interval", "retries"},
}

var knownInstructions = map[string]bool{
	"from": true, "run": true, "cmd": true, "label": true, "maintainer": true,
	"expose": true, "env": true, "add": true, "copy": true, "entrypoint": true,
	"volume": true, "user": true, "workdir": true, "arg": true, "onbuild": true,
	"stopsignal": true, "healthcheck": true, "shell": true,
}

// parseContainerfile parses r into stages.
func parseContainerfile(r io.Reader) (*containerfile, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	escape := '\\'
	directives := true
	var (
		cf      containerfile
		buf     strings.Builder
		start   int
		pending bool
		lineNo  int
	)
	flush := func() error {
		if !pending {
			return nil
		}
		pending = false
		text := strings.TrimSpace(buf.String())
		buf.Reset()
		if text == "" {
			return nil
		}
		ins, err := parseInstruction(text, start)
		if err != nil {
			return err
		}
		switch {
		case ins.Cmd == "from":
			stage := cfStage{From: ins}
			words := strings.Fields(ins.Rest)
			switch {
			case len(words) == 3 && strings.EqualFold(words[1], "as"):
				stage.Name = strings.ToLower(words[2])
			case len(words) != 1:
				return fmt.Errorf("line %d: FROM takes an image and an optional AS name", ins.Line)
			}
			cf.Stages = append(cf.Stages, stage)
		case len(cf.Stages) == 0:
			if ins.Cmd != "arg" {
				return fmt.Errorf("line %d: %s before the first FROM", ins.Line, strings.ToUpper(ins.Cmd))
			}
			cf.GlobalArgs = append(cf.GlobalArgs, ins)
		default:
			s := &cf.Stages[len(cf.Stages)-1]
			s.Instructions = append(s.Instructions, ins)
		}
		return nil
	}
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if lineNo == 1 {
			line = strings.TrimPrefix(line, "\ufeff")
		}
		trimmed := strings.TrimSpace(line)
		if directives {
			if m := directiveRE.FindStringSubmatch(trimmed); m != nil {
				if strings.EqualFold(m[1], "escape") {
					switch m[2] {
					case "\\":
						escape = '\\'
					case "`":
						escape = '`'
					default:
						return nil, fmt.Errorf("line %d: invalid escape directive %q", lineNo, m[2])
					}
				}
				continue
			}
			directives = false
		}
		// Comments and blank lines are dropped, inside continuations too.
		if strings.HasPrefix(trimmed, "#") || trimmed == "" {
			continue
		}
		if !pending {
			pending = true
			start = lineNo
		}
		body := strings.TrimRightFunc(line, unicode.IsSpace)
		if strings.HasSuffix(body, string(escape)) {
			buf.WriteString(body[:len(body)-1])
			continue
		}
		buf.WriteString(line)
		if err := flush(); err != nil {
			return nil, err
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(cf.Stages) == 0 {
		return nil, fmt.Errorf("no FROM instruction")
	}
	cf.Escape = escape
	return &cf, nil
}

func parseInstruction(text string, line int) (cfInstruction, error) {
	ins := cfInstruction{Line: line, Original: text, Flags: map[string]string{}}
	var keyword, rest string
	if i := strings.IndexFunc(text, unicode.IsSpace); i >= 0 {
		keyword, rest = text[:i], text[i:]
	} else {
		keyword, rest = text, ""
	}
	ins.Cmd = strings.ToLower(keyword)
	if !knownInstructions[ins.Cmd] {
		return ins, fmt.Errorf("line %d: unknown instruction %s", line, keyword)
	}
	rest = strings.TrimSpace(rest)
	if allowed, ok := instructionFlags[ins.Cmd]; ok {
		for strings.HasPrefix(rest, "--") {
			word := rest
			if i := strings.IndexFunc(rest, unicode.IsSpace); i >= 0 {
				word, rest = rest[:i], strings.TrimSpace(rest[i:])
			} else {
				rest = ""
			}
			name, value, hasValue := strings.Cut(word[2:], "=")
			if !contains(allowed, name) {
				if ins.Cmd == "run" && (name == "mount" || name == "security") {
					return ins, fmt.Errorf("line %d: RUN --%s is not supported by spod build", line, name)
				}
				return ins, fmt.Errorf("line %d: unknown flag --%s for %s", line, name, strings.ToUpper(ins.Cmd))
			}
			if !hasValue && name != "link" {
				return ins, fmt.Errorf("line %d: --%s needs a value", line, name)
			}
			ins.Flags[name] = value
		}
	}
	if (ins.Cmd == "run" || ins.Cmd == "copy" || ins.Cmd == "add") && heredocRE.MatchString(rest) {
		return ins, fmt.Errorf("line %d: heredocs are not supported by spod build", line)
	}
	ins.Rest = rest
	switch ins.Cmd {
	case "run", "cmd", "entrypoint", "shell", "copy", "add", "volume":
		if strings.HasPrefix(rest, "[") {
			var args []string
			if err := json.Unmarshal([]byte(rest), &args); err == nil {
				ins.JSON, ins.IsJSON = args, true
			} else if ins.Cmd == "shell" {
				return ins, fmt.Errorf("line %d: SHELL needs a JSON array", line)
			}
		}
	}
	if ins.Cmd == "shell" && !ins.IsJSON {
		return ins, fmt.Errorf("line %d: SHELL needs a JSON array", line)
	}
	return ins, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// expandWords applies shell-style quoting and ${VAR} expansion to s and,
// when split is true, splits it into words on unquoted whitespace (expanded
// values are not re-split, so ENV A=$B keeps spaces in B). Supported
// forms: $VAR, ${VAR}, ${VAR:-word}, ${VAR-word}, ${VAR:+word}, ${VAR+word}.
func expandWords(s string, lookup func(string) (string, bool), escape rune, split bool) ([]string, error) {
	var (
		words   []string
		cur     strings.Builder
		started bool
	)
	runes := []rune(s)
	emit := func() {
		if started {
			words = append(words, cur.String())
		}
		cur.Reset()
		started = false
	}
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case split && unicode.IsSpace(c):
			emit()
		case c == escape:
			started = true
			if i+1 < len(runes) {
				i++
				cur.WriteRune(runes[i])
			} else {
				cur.WriteRune(c)
			}
		case c == '\'':
			started = true
			end := indexRune(runes, i+1, '\'')
			if end < 0 {
				return nil, fmt.Errorf("unterminated single quote in %q", s)
			}
			cur.WriteString(string(runes[i+1 : end]))
			i = end
		case c == '"':
			started = true
			i++
			for ; i < len(runes) && runes[i] != '"'; i++ {
				switch {
				case runes[i] == escape && i+1 < len(runes) && (runes[i+1] == '"' || runes[i+1] == '$' || runes[i+1] == escape):
					i++
					cur.WriteRune(runes[i])
				case runes[i] == '$':
					n, err := expandVar(runes, i, lookup, escape, &cur)
					if err != nil {
						return nil, err
					}
					i = n
				default:
					cur.WriteRune(runes[i])
				}
			}
			if i >= len(runes) {
				return nil, fmt.Errorf("unterminated double quote in %q", s)
			}
		case c == '$':
			// An unquoted expansion to nothing does not make a word.
			before := cur.Len()
			n, err := expandVar(runes, i, lookup, escape, &cur)
			if err != nil {
				return nil, err
			}
			started = started || cur.Len() > before
			i = n
		default:
			started = true
			cur.WriteRune(c)
		}
	}
	emit()
	return words, nil
}

// expandWord expands s without splitting it.
func expandWord(s string, lookup func(string) (string, bool), escape rune) (string, error) {
	words, err := expandWords(s, lookup, escape, false)
	if err != nil || len(words) == 0 {
		return "", err
	}
	return words[0], nil
}

func indexRune(runes []rune, from int, r rune) int {
	for i := from; i < len(runes); i++ {
		if runes[i] == r {
			return i
		}
	}
	return -1
}

func isNameRune(r rune) bool {
	return r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// expandVar expands the variable starting at runes[i] ('$') into out and
// returns the index of its last rune.
func expandVar(runes []rune, i int, lookup func(string) (string, bool), escape rune, out *strings.Builder) (int, error) {
	if i+1 >= len(runes) {
		out.WriteRune('$')
		return i, nil
	}
	if runes[i+1] != '{' {
		j := i + 1
		for j < len(runes) && isNameRune(runes[j]) {
			j++
		}
		if j == i+1 {
			out.WriteRune('$')
			return i, nil
		}
		v, _ := lookup(string(runes[i+1 : j]))
		out.WriteString(v)
		return j - 1, nil
	}
	end := -1
	depth := 0
	for j := i + 2; j < len(runes); j++ {
		if runes[j] == escape {
			j++
			continue
		}
		if runes[j] == '{' {
			depth++
		} else if runes[j] == '}' {
			if depth == 0 {
				end = j
				break
			}
			depth--
		}
	}
	if end < 0 {
		return 0, fmt.Errorf("missing '}' in %q", string(runes))
	}
	body := string(runes[i+2 : end])
	nameEnd := 0
	for nameEnd < len(body) && isNameRune(rune(body[nameEnd])) {
		nameEnd++
	}
	name, op := body[:nameEnd], body[nameEnd:]
	if name == "" {
		return 0, fmt.Errorf("bad substitution ${%s}", body)
	}
	v, set := lookup(name)
	word := func(w string) (string, error) { return expandWord(w, lookup, escape) }
	switch {
	case op == "":
		out.WriteString(v)
	case strings.HasPrefix(op, ":-"), strings.HasPrefix(op, "-"):
		colon := strings.HasPrefix(op, ":")
		if !set || colon && v == "" {
			w, err := word(strings.TrimPrefix(strings.TrimPrefix(op, ":"), "-"))
			if err != nil {
				return 0, err
			}
			v = w
		}
		out.WriteString(v)
	case strings.HasPrefix(op, ":+"), strings.HasPrefix(op, "+"):
		colon := strings.HasPrefix(op, ":")
		if set && (!colon || v != "") {
			w, err := word(strings.TrimPrefix(strings.TrimPrefix(op, ":"), "+"))
			if err != nil {
				return 0, err
			}
			out.WriteString(w)
		}
	default:
		return 0, fmt.Errorf("unsupported substitution ${%s}", body)
	}
	return end, nil
}

// keyValues parses ENV, LABEL, and ARG arguments: name=value pairs, or for
// ENV and LABEL the legacy single "name value" form.
func keyValues(ins cfInstruction, lookup func(string) (string, bool), escape rune) ([][2]string, error) {
	rest := ins.Rest
	if rest == "" {
		return nil, fmt.Errorf("line %d: %s needs arguments", ins.Line, strings.ToUpper(ins.Cmd))
	}
	first := rest
	if i := strings.IndexFunc(rest, unicode.IsSpace); i >= 0 {
		first = rest[:i]
	}
	if ins.Cmd != "arg" && !strings.Contains(first, "=") {
		value, err := expandWord(strings.TrimSpace(rest[len(first):]), lookup, escape)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", ins.Line, err)
		}
		return [][2]string{{first, value}}, nil
	}
	words, err := expandWords(rest, lookup, escape, true)
	if err != nil {
		return nil, fmt.Errorf("line %d: %w", ins.Line, err)
	}
	var out [][2]string
	for _, w := range words {
		k, v, ok := strings.Cut(w, "=")
		if k == "" {
			return nil, fmt.Errorf("line %d: invalid %s %q", ins.Line, strings.ToUpper(ins.Cmd), w)
		}
		if !ok {
			if ins.Cmd != "arg" {
				return nil, fmt.Errorf("line %d: %s %q needs name=value", ins.Line, strings.ToUpper(ins.Cmd), w)
			}
			// An ARG without a default: marked unset by the NUL value.
			v = "\x00"
		}
		out = append(out, [2]string{k, v})
	}
	return out, nil
}

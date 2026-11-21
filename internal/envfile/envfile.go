// Package envfile reads a .env file: the values personal to a home, such as a login's username
// and password, kept beside its go.mod and out of Git.
//
// A line is NAME=VALUE, with an optional "export " before it, as a shell would take it. Blank lines
// and lines starting with # are skipped. A value may be in double quotes, where \n, \r, \t, \", \\
// and \$ are escapes, or in single quotes, where nothing is, and either may span lines. An unquoted
// value ends at the line's end or at a # after a space, and its spaces at either end are dropped.
// Nothing is expanded: a $ is a $, so a password needs no escaping. A name follows
// ident.VarRule, and a name given twice takes its last value.
package envfile

import (
	"errors"
	"fmt"
	"strings"

	"oikade.dev/internal/ident"
)

// Parse reads a file's contents. An error starts with the line it's on, such as "3: …".
func Parse(s string) (map[string]string, error) {
	p := parser{s: strings.TrimPrefix(strings.ReplaceAll(s, "\r\n", "\n"), "\ufeff"), line: 1}
	values := map[string]string{}
	for !p.done() {
		start := p.line
		name, value, ok, err := p.entry()
		if err != nil {
			return nil, fmt.Errorf("%d: %w", start, err)
		}
		if ok {
			values[name] = value
		}
	}
	return values, nil
}

type parser struct {
	s    string
	line int
}

func (p *parser) done() bool { return p.s == "" }

// rest is what's left of the line, without its newline, which it takes.
func (p *parser) rest() string {
	line, after, found := strings.Cut(p.s, "\n")
	p.s = after
	if found {
		p.line++
	}
	return line
}

// entry reads a line, or more for a quoted value that spans them. ok is false for a blank line or a
// comment.
func (p *parser) entry() (name, value string, ok bool, err error) {
	p.s = strings.TrimLeft(p.s, " \t")
	if strings.HasPrefix(p.s, "#") || strings.HasPrefix(p.s, "\n") || p.s == "" {
		p.rest()
		return "", "", false, nil
	}
	if after, found := strings.CutPrefix(p.s, "export"); found && (strings.HasPrefix(after, " ") || strings.HasPrefix(after, "\t")) {
		p.s = strings.TrimLeft(after, " \t")
	}
	eq := strings.IndexAny(p.s, "=\n")
	if eq < 0 || p.s[eq] != '=' {
		// Only a name is quoted: the line may be part of a value, such as a password that should
		// have been in quotes.
		if line := strings.TrimSpace(p.rest()); ident.ValidVarName(line) {
			return "", "", false, fmt.Errorf("%q has no =: write NAME=VALUE", line)
		}
		return "", "", false, errors.New("the line has no =: write NAME=VALUE")
	}
	name = strings.TrimRight(p.s[:eq], " \t")
	if err := ident.CheckVarName(name); err != nil {
		p.rest()
		return "", "", false, err
	}
	after := p.s[eq+1:]
	p.s = strings.TrimLeft(after, " \t")
	switch {
	case strings.HasPrefix(p.s, "#") && len(p.s) < len(after):
		// NAME= # a comment
		p.rest()
		return name, "", true, nil
	case strings.HasPrefix(p.s, `"`):
		value, err = p.quoted('"')
	case strings.HasPrefix(p.s, "'"):
		value, err = p.quoted('\'')
	default:
		value = unquoted(p.rest())
		return name, value, true, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("%s: %w", name, err)
	}
	if tail := strings.TrimSpace(p.rest()); tail != "" && !strings.HasPrefix(tail, "#") {
		return "", "", false, fmt.Errorf("%s has more after its closing quote, where only a comment can go", name)
	}
	return name, value, true, nil
}

// quoted reads a value in quotes, which p.s starts with, up to the closing one.
func (p *parser) quoted(quote byte) (string, error) {
	var b strings.Builder
	for i := 1; i < len(p.s); i++ {
		c := p.s[i]
		switch {
		case c == quote:
			p.s = p.s[i+1:]
			return b.String(), nil
		case c == '\n':
			p.line++
			b.WriteByte(c)
		case c == '\\' && quote == '"' && i+1 < len(p.s):
			i++
			switch e := p.s[i]; e {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '"', '\\', '$':
				b.WriteByte(e)
			default:
				// An escape that isn't one is the backslash and the character, as a shell keeps them.
				if e == '\n' {
					p.line++
				}
				b.WriteByte('\\')
				b.WriteByte(e)
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", fmt.Errorf("its value has no closing %c", quote)
}

// unquoted is a value without quotes: up to a # after a space, without spaces at either end.
func unquoted(s string) string {
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			s = s[:i]
			break
		}
	}
	return strings.TrimSpace(s)
}

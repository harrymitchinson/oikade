// Package ident names the things a home is made of.
package ident

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// ID names a Device, rule or plugin. The host's store keeps saved state under its String form, so
// changing that form loses the state.
type ID struct {
	Name string
}

func (id ID) String() string {
	return id.Name
}

// LogValue logs an ID as its name, so a filter on a device's or a plugin's name finds every line
// that names it.
func (id ID) LogValue() slog.Value {
	return slog.StringValue(id.Name)
}

// Kind is a kind of thing a home is made of, as a name's errors and suggestions call it.
type Kind string

// The kinds whose names Oikade owns, and so checks.
const (
	Device Kind = "device"
	Room   Kind = "room"
	Group  Kind = "group"
	Case   Kind = "case"
	Flow   Kind = "flow"
	Plugin Kind = "plugin"
	Client Kind = "client"
	Signal Kind = "signal"
)

const MaxName = 63

// Rule says what a name may be, as an error says it.
const Rule = "use lowercase letters, digits and single dashes, starting with a letter"

var validName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// ValidName reports whether name follows the rule every name Oikade owns does: up to MaxName
// lowercase letters, digits and single dashes, starting with a letter. Such a name needs no
// escaping anywhere: in a URI, a topic, a file name or a shell.
func ValidName(name string) bool {
	return len(name) <= MaxName && validName.MatchString(name)
}

// CheckName refuses a name of kind k that doesn't follow the rule, with the name it would make, if
// there is one.
func CheckName(k Kind, name string) error {
	switch {
	case name == "":
		return fmt.Errorf("a %s needs a name", k)
	case ValidName(name):
		return nil
	}
	if suggested := Slug(k, name); suggested != "" {
		return fmt.Errorf("%q isn't a %s name: %s, such as %q", name, k, Rule, suggested)
	}
	return fmt.Errorf("%q isn't a %s name: %s", name, k, Rule)
}

// Slug makes a name of kind k that follows the rule from what an app calls something, such as
// "living-room" from "Living Room" or "entree" from "Entrée". It starts with the kind if what's left
// doesn't start with a letter, and is empty if s has no digits and no letters it can spell in a to z,
// such as a name in Cyrillic.
func Slug(k Kind, s string) string {
	slug := dashed(s)
	if slug == "" {
		return ""
	}
	if !unicode.IsLetter(rune(slug[0])) {
		slug = string(k) + "-" + slug
	}
	return strings.TrimRight(slug[:min(len(slug), MaxName)], "-")
}

// unsplit spells the letters that Unicode doesn't split into a letter and an accent.
var unsplit = map[rune]string{
	'œ': "oe", 'æ': "ae", 'ß': "ss", 'ø': "o", 'ł': "l", 'đ': "d", 'ð': "d", 'þ': "th", 'ı': "i", 'ħ': "h", 'ŧ': "t",
}

// dashed is s in lowercase letters and digits, each letter without its accent, with a dash for each
// run of anything else between them.
func dashed(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(norm.NFKD.String(s)) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
		case unsplit[c] != "":
			b.WriteString(unsplit[c])
		case unicode.Is(unicode.Mn, c):
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	return strings.TrimRight(b.String(), "-")
}

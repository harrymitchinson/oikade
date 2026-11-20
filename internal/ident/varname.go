package ident

import (
	"fmt"
	"regexp"
	"strings"
)

// VarRule says what the name of a value in your .env may be, as an error says it. It's an
// environment variable's rule, so a name is the same in your code, the file and the environment.
const VarRule = "use upper-case letters, digits and underscores, starting with a letter"

var validVarName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// ValidVarName reports whether name follows VarRule.
func ValidVarName(name string) bool {
	return validVarName.MatchString(name)
}

// CheckVarName refuses a value's name that doesn't follow VarRule, with the name it would make, if
// there is one.
func CheckVarName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("a variable needs a name: %s", VarRule)
	case ValidVarName(name):
		return nil
	}
	if suggested := VarName(name); suggested != "" {
		return fmt.Errorf("%q isn't a variable name: %s, such as %q", name, VarRule, suggested)
	}
	return fmt.Errorf("%q isn't a variable name: %s", name, VarRule)
}

// VarName makes a name that follows VarRule from s, such as "TOSHIBA_USER" from "toshiba-user". It
// starts with "VAR_" if what's left doesn't start with a letter, and is empty if s has no letters or
// digits.
func VarName(s string) string {
	var b strings.Builder
	for _, c := range strings.ToUpper(s) {
		switch {
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteRune(c)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "_"):
			b.WriteByte('_')
		}
	}
	name := strings.TrimRight(b.String(), "_")
	if name != "" && (name[0] < 'A' || name[0] > 'Z') {
		name = "VAR_" + name
	}
	return name
}

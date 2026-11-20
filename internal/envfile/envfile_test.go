package envfile

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	file := `# The real home's accounts
TOSHIBA_USER=someone
export TOSHIBA_PASSWORD="pa ss\"word\n"

	TAHOMA_ADDRESS = gateway-1234.local:8443   # the hub on the LAN
TAHOMA_PIN='12$34 # not a comment'
EMPTY=
COMMENTED= # nothing
HASH=ab#cd
DOLLAR=$HOME
BACKSLASH="a\qb"
MULTI="line one
line two"
SINGLE='no \n escapes'
REPEATED=first
REPEATED=last
exported=no
`
	// The last is an error, so drop it for this test.
	file = strings.TrimSuffix(file, "exported=no\n")
	got, err := Parse(file)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"TOSHIBA_USER":     "someone",
		"TOSHIBA_PASSWORD": "pa ss\"word\n",
		"TAHOMA_ADDRESS":   "gateway-1234.local:8443",
		"TAHOMA_PIN":       "12$34 # not a comment",
		"EMPTY":            "",
		"COMMENTED":        "",
		"HASH":             "ab#cd",
		"DOLLAR":           "$HOME",
		"BACKSLASH":        `a\qb`,
		"MULTI":            "line one\nline two",
		"SINGLE":           `no \n escapes`,
		"REPEATED":         "last",
	}
	if !maps.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestParseTakesWindowsLineEndingsAndAByteOrderMark(t *testing.T) {
	got, err := Parse("\xef\xbb\xbfA=1\r\nB=\"two\"\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"A": "1", "B": "two"}; !maps.Equal(got, want) {
		t.Errorf("got %q", got)
	}
}

func TestParseEmpty(t *testing.T) {
	for _, s := range []string{"", "\n\n", "# only a comment\n", "   \n\t\n"} {
		got, err := Parse(s)
		if err != nil || len(got) != 0 {
			t.Errorf("Parse(%q) = %q, %v", s, got, err)
		}
	}
}

func TestParseSaysWhichLineIsWrong(t *testing.T) {
	for file, want := range map[string]string{
		"A=1\ntoshiba-user=someone\n": `2: "toshiba-user" isn't a variable name: use upper-case letters, digits and underscores, starting with a letter, such as "TOSHIBA_USER"`,
		"A=1\n\nJUST_A_NAME\n":         `3: "JUST_A_NAME" has no =: write NAME=VALUE`,
		"=value\n":                     "1: a variable needs a name: use upper-case letters, digits and underscores, starting with a letter",
		"A=\"open\nB=2\n":              "1: A: its value has no closing \"",
		"A='open\n":                    "1: A: its value has no closing '",
		"A=\"x\" y\n":                  `1: A has "y" after its closing quote`,
		"A=\"two\nlines\"\nb=1\n":      `3: "b" isn't a variable name: use upper-case letters, digits and underscores, starting with a letter, such as "B"`,
	} {
		_, err := Parse(file)
		if err == nil || err.Error() != want {
			t.Errorf("Parse(%q) = %v, want %s", file, err, want)
		}
	}
}

func TestReadNamesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("A=1\nbad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err == nil || !strings.HasPrefix(err.Error(), path+":2: ") {
		t.Errorf("got %v", err)
	}
}

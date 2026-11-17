package hometest_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The test in the package doc, the README's and the one oikade init writes are examples that run:
// each one's body is in example_test.go, line for line.
func TestTheDocsTestsAreExamples(t *testing.T) {
	examples := statements(read(t, "example_test.go"))
	examples = strings.ReplaceAll(examples, `hometest.Options{Types: "devices_test.go"}`, "hometest.Options{}")
	for name, doc := range map[string]string{
		"the package doc":        commented(read(t, "hometest.go")),
		"the README":             read(t, "../README.md"),
		"oikade init's scaffold": read(t, "../cmd/oikade/home.go"),
	} {
		body := testBody(t, name, doc)
		if !strings.Contains(examples, body) {
			t.Errorf("%s's test isn't an example in example_test.go:\n%s", name, body)
		}
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// commented is the code in a doc comment, without its comment markers.
func commented(src string) string {
	var out []string
	for line := range strings.Lines(src) {
		if code, ok := strings.CutPrefix(line, "//\t"); ok {
			out = append(out, code)
		} else if strings.TrimSpace(line) == "//" {
			out = append(out, "\n")
		}
	}
	return strings.Join(out, "")
}

// testFunc is a test that starts a simulated home, up to the brace that ends it.
var testFunc = regexp.MustCompile(`(?s)func Test\w+\(t \*testing\.T\) \{\n(\s*h := hometest\.New.*?)\n\}`)

// testBody is the statements of the test in doc.
func testBody(t *testing.T, name, doc string) string {
	t.Helper()
	m := testFunc.FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("%s has no test of a simulated home", name)
	}
	return statements(m[1])
}

// statements are src's lines, trimmed, without the empty ones.
func statements(src string) string {
	var out []string
	for line := range strings.Lines(src) {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

package apicheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func checkSource(t *testing.T, src string) []string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "api.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := Check(dir)
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestCheckFindsInternalTypesInExportedAPI(t *testing.T) {
	const header = "package api\nimport \"example.com/m/internal/thing\"\nvar _ thing.X\n"
	for name, src := range map[string]string{
		"parameter":        "func F(x thing.T) {}",
		"result":           "func F() thing.T { panic(0) }",
		"method":           "type S struct{}\nfunc (S) M() *thing.T { return nil }",
		"field":            "type S struct{ F thing.T }",
		"embedded field":   "type S struct{ thing.T }",
		"nested in map":    "type S struct{ F map[string][]thing.T }",
		"alias":            "type A = thing.T",
		"interface method": "type I interface{ M(thing.T) }",
		"variable":         "var V thing.T",
		"func type":        "type F func(thing.T) error",
	} {
		t.Run(name, func(t *testing.T) {
			found := checkSource(t, header+src)
			if len(found) != 1 || !strings.Contains(found[0], "example.com/m/internal/thing") {
				t.Fatalf("got %q, want one mention of the internal package", found)
			}
		})
	}
}

func TestCheckIgnoresWhatIsNotAPI(t *testing.T) {
	found := checkSource(t, `package api
import (
	t "example.com/m/internal/thing"
	"example.com/m/public"
)
type s struct{ F t.T }
type S struct {
	f t.T
	P public.T
}
func f(x t.T) {}
func (s) M(x t.T) {}
func (S) m(x t.T) {}
func F() { var _ t.T }
`)
	if len(found) != 0 {
		t.Fatalf("got %q, want none", found)
	}
}

func TestCheckUsesAnImportsLocalName(t *testing.T) {
	found := checkSource(t, "package api\nimport named \"example.com/m/internal/thing\"\nfunc F(named.T) {}\n")
	if len(found) != 1 {
		t.Fatalf("got %q, want one", found)
	}
}

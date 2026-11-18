package testkit

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// update rewrites the golden files a test compares with, rather than comparing.
var update = flag.Bool("update", false, "rewrite testdata/*.golden with what the tests print")

// Golden fails t unless got is what testdata/NAME.golden holds. With -update, it writes got there
// instead, to read before committing.
func Golden(t testing.TB, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v: run the test with -update to write it", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs; run the test with -update and read the diff\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}

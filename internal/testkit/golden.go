package testkit

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// Update is -update, which has a test rewrite the files it compares with, such as golden files,
// rather than compare. It's testkit's, so that every package's tests share one flag.
var Update = flag.Bool("update", false, "rewrite the files tests compare with, such as testdata/*.golden")

// Golden fails t unless got is what testdata/NAME.golden holds. With -update, it writes got there
// instead, to read before committing.
func Golden(t testing.TB, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *Update {
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

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
)

// Every verb, given each kind of ref, a ref that doesn't parse, and a ref that names nothing, exits as
// the design gives: 2 for a command typed wrong, such as a kind the verb doesn't take, and 1 for the
// rest, such as a ref that names nothing on record. The kinds a verb takes are oikade help KIND's, so
// help and the verbs can't drift apart.
func TestEveryVerbExitsAsTheGrammarSays(t *testing.T) {
	dir := aHostWithNothing(t)
	stdinOf(t, "a-value\n")
	refs := map[string]string{
		"device": "device/nothing", "capability": "device/nothing/on", "room": "room/nothing",
		"case": "case/nothing/on/never", "flow": "flow/nothing", "plugin": "plugin/nothing",
		"client": "client/nothing", "matter": "matter",
	}
	// A bare word is a verb's main kind: a device for status, history, tail and forget, and a plugin
	// for setup. Every other verb needs a ref.
	bare := map[string]bool{"status": true, "history": true, "tail": true, "forget": true, "setup": true}
	// The verbs that take one ref. A second is a command typed wrong.
	one := map[string]bool{"set": true, "pause": true, "resume": true, "pair": true, "setup": true}
	run := func(args ...string) int {
		_, _, code := runOikade(t, cli.Plain, append(args, "--data-dir", dir)...)
		return code
	}
	// namesNothing is what a verb exits given something of a kind it takes that isn't on record: 1,
	// but for history, as the log outlives a record. tail follows, so -1 holds only that it isn't 2.
	namesNothing := func(verb string) int {
		switch verb {
		case "history":
			return 0
		case "tail":
			return -1
		}
		return 1
	}
	check := func(what string, got, want int) {
		t.Helper()
		if want == -1 && got == cli.ExitUsage || want != -1 && got != want {
			t.Errorf("%s exits %d, not %d", what, got, want)
		}
	}
	verbs := []string{"list", "status", "history", "tail", "set", "pause", "resume", "forget", "remove", "setup", "pair"}
	for _, verb := range verbs {
		for _, k := range kinds {
			ref, ok := refs[k.word]
			if !ok {
				continue
			}
			args := []string{verb, ref}
			if verb == "set" {
				args = append(args, "true")
			}
			want := cli.ExitUsage
			if k.takes(verb) {
				want = namesNothing(verb)
			}
			check("oikade "+strings.Join(args, " "), run(args...), want)
			if one[verb] && k.takes(verb) {
				check("oikade "+strings.Join(args, " ")+" "+ref, run(append(args, ref)...), cli.ExitUsage)
			}
		}
		want := cli.ExitUsage
		if bare[verb] {
			want = namesNothing(verb)
		}
		check("oikade "+verb+" nothing", run(verb, "nothing"), want)
		check("oikade "+verb+" Bad/Ref", run(verb, "Bad/Ref"), cli.ExitUsage)
	}
	// A rule's name on its own is typed wrong whether or not a host is there to say the rule's ref.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, _, code := runOikade(t, cli.Plain, "pause", "nothing", "--data-dir", t.TempDir())
	check("oikade pause nothing, with no host", code, cli.ExitUsage)
}

// Every kind's help names commands that exist, and each takes a thing of its kind as help says.
func TestEveryKindsHelpNamesItsCommands(t *testing.T) {
	for _, k := range kinds {
		if len(k.commands) == 0 || k.about == "" {
			t.Errorf("%s has no help", k.word)
		}
		for _, c := range k.commands {
			fields := strings.Fields(c.Name)
			if fields[0] != "oikade" {
				continue
			}
			if _, ok := find(fields[1]); !ok {
				t.Errorf("%s: no oikade %s", k.word, fields[1])
			}
		}
	}
}

// stdinOf makes os.Stdin read s, as a script piping a secret does.
func stdinOf(t *testing.T, s string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = saved
		_ = f.Close()
	})
}

// aHostWithNothing is a host with nothing on record: an empty status, no clients, an empty
// log, and an error for anything asked of something.
func aHostWithNothing(t *testing.T) (dir string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "oik") //nolint:usetesting // a Unix socket's path has to fit in about 100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, api.SocketName))
	if err != nil {
		t.Fatal(err)
	}
	answer := func(resp api.Response) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", api.ContentType)
			_ = json.NewEncoder(w).Encode(resp)
		}
	}
	mux := http.NewServeMux()
	// A process is running, so the host can say what its rules are: none.
	mux.HandleFunc("GET "+api.Prefix+"/status", answer(api.Response{Status: &api.Status{Home: api.Home{Process: true}}}))
	mux.HandleFunc("GET "+api.Prefix+"/clients", answer(api.Response{}))
	stream := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", api.StreamContentType)
		out := json.NewEncoder(w)
		_ = out.Encode(api.EventLog{})
		_ = out.Encode(api.LogEnd(""))
	}
	mux.HandleFunc("GET "+api.Prefix+"/events", stream)
	mux.HandleFunc("GET "+api.Prefix+"/tail", stream)
	mux.HandleFunc("POST /", answer(api.Response{Error: "there's nothing of that name on record"}))
	host := &http.Server{Handler: mux}
	go func() { _ = host.Serve(l) }()
	t.Cleanup(func() { _ = host.Close() })
	return dir
}

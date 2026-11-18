package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/ident"
)

// redirectWait is how long a sign-in in the browser may take.
const redirectWait = 5 * time.Minute

func runSetup(ctx context.Context, args []string) error {
	fs := newFlags("setup")
	target := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print how it ended as JSON, whose fields stay stable for scripts.")
	words := cli.Parse(fs, args)
	if len(words) != 1 {
		return cli.UsageErrorf("oikade setup takes the plugin to set up")
	}
	transport, host := target.resolve()
	s := setupper{
		stdout: os.Stdout, stderr: os.Stderr, in: bufio.NewReader(os.Stdin), terminal: os.Stdin,
		browse: browse, catch: catchRedirect,
	}
	plugin, err := nameOf(ident.Plugin, words[0])
	if err != nil {
		return err
	}
	return s.run(ctx, transport, host, plugin, *asJSON)
}

// setupper runs a plugin's setup: it asks the person what the plugin asks, opens its pages and
// catches their redirects.
type setupper struct {
	stdout, stderr io.Writer
	in             *bufio.Reader
	// terminal, if it is one, is where a secret is read without echoing.
	terminal *os.File
	// browse opens a page. It may fail, as on a machine with no browser: the page is shown anyway.
	browse func(page string) error
	// catch listens on an open's redirect and returns the query it's called with.
	catch func(ctx context.Context, redirect string, listening func()) (string, error)
}

func (s setupper) run(ctx context.Context, t api.Transport, host, plugin string, asJSON bool) error {
	_, _ = fmt.Fprintf(s.stderr, "Starting the setup of %s on %s\n", plugin, host)
	var session string
	var done *contractv1.SetupDone
	err := api.Setup(ctx, t, plugin, func(step api.SetupStep) error {
		switch {
		case step.Seq == 0 && step.Done == nil:
			session = step.Session
		case step.Say != "":
			_, _ = fmt.Fprintln(s.stderr, step.Say)
		case step.Kept != "":
			_, _ = fmt.Fprintf(s.stderr, "Kept secret %s on the host\n", step.Kept)
		case step.Ask != nil:
			answer, err := s.ask(ctx, *step.Ask)
			if err != nil {
				return err
			}
			return api.SetupReply(ctx, t, plugin, session, step.Seq, answer)
		case step.Open != nil:
			query, err := s.open(ctx, *step.Open)
			if err != nil {
				return err
			}
			return api.SetupReply(ctx, t, plugin, session, step.Seq, query)
		case step.Done != nil:
			done = step.Done
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, api.ErrHostStopped) {
			return fmt.Errorf("the host stopped before the setup of %s finished", plugin)
		}
		return err
	}
	if asJSON {
		if err := cli.PrintJSON(struct {
			OK     bool   `json:"ok"`
			Plugin string `json:"plugin"`
			Result string `json:"result,omitempty"`
			Error  string `json:"error,omitempty"`
		}{done.Error == "", plugin, done.Result, done.Error}); err != nil {
			return err
		}
	} else if done.Error == "" {
		_, _ = fmt.Fprintln(s.stdout, done.Result)
	}
	if done.Error != "" {
		return fmt.Errorf("%s isn't set up: %s", plugin, done.Error)
	}
	return nil
}

// ask asks the person, on stderr, and reads a line. A secret isn't echoed on a terminal. Ending ctx,
// as Ctrl-C does, stops waiting, and puts the terminal back as it was.
func (s setupper) ask(ctx context.Context, a contractv1.SetupAsk) (string, error) {
	_, _ = fmt.Fprintf(s.stderr, "%s: ", a.Prompt)
	type read struct {
		value string
		err   error
	}
	got := make(chan read, 1)
	restore := func() {}
	if a.Secret && s.terminal != nil && term.IsTerminal(int(s.terminal.Fd())) {
		fd := int(s.terminal.Fd())
		if state, err := term.GetState(fd); err == nil {
			restore = func() { _ = term.Restore(fd, state) }
		}
		go func() {
			value, err := term.ReadPassword(fd)
			got <- read{string(value), err}
		}()
	} else {
		go func() {
			value, err := s.readLine(a.Prompt)
			got <- read{value, err}
		}()
	}
	select {
	case r := <-got:
		if a.Secret && s.terminal != nil && term.IsTerminal(int(s.terminal.Fd())) {
			_, _ = fmt.Fprintln(s.stderr)
		}
		return r.value, r.err
	case <-ctx.Done():
		restore()
		_, _ = fmt.Fprintln(s.stderr)
		return "", ctx.Err()
	}
}

// readLine reads one answer.
func (s setupper) readLine(prompt string) (string, error) {
	line, err := s.in.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("no answer to %q: stdin ended", prompt)
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// open shows a page in the browser and waits for its redirect, which carries the answer.
func (s setupper) open(ctx context.Context, o contractv1.SetupOpen) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, redirectWait)
	defer cancel()
	return s.catch(ctx, o.Redirect, func() {
		_, _ = fmt.Fprintf(s.stderr, "Opening %s\n", o.URL)
		if err := s.browse(o.URL); err != nil {
			_, _ = fmt.Fprintln(s.stderr, "Could not open a browser, so open that page yourself")
		}
		_, _ = fmt.Fprintf(s.stderr, "Waiting for the page to send you back to %s\n", o.Redirect)
	})
}

// browse opens a page in this machine's browser, or with $BROWSER, a command and its arguments, if
// it's set.
func browse(page string) error {
	command := []string{"xdg-open"}
	if runtime.GOOS == "darwin" {
		command = []string{"open"}
	}
	if b := strings.Fields(os.Getenv("BROWSER")); len(b) > 0 {
		command = b
	}
	cmd := exec.Command(command[0], append(command[1:], page)...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// catchRedirect listens on redirect, which is on this machine, calls listening once it does, and
// returns the query of the first request to its path.
func catchRedirect(ctx context.Context, redirect string, listening func()) (string, error) {
	address, err := contractv1.RedirectAddress(redirect)
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(redirect)
	path := u.Path
	if path == "" {
		path = "/"
	}
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		return "", cli.Wrap(err, "couldn't listen for the page's redirect on %s", address).
			WithHint("something else has that port: stop it, and run oikade setup again")
	}
	got := make(chan string, 1)
	page := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(w, "Done. You can close this tab and go back to the terminal.")
		select {
		case got <- r.URL.RawQuery:
		default:
		}
	}
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(page)}
	go func() { _ = srv.Serve(l) }()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	listening()
	select {
	case q := <-got:
		return q, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("the page didn't send you back to %s within %s", redirect, redirectWait)
		}
		return "", ctx.Err()
	}
}

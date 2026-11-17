package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
)

// freePort is a port on 127.0.0.1 nothing listens on, for now. catchRedirect binds the redirect's
// port itself, so the port is let go first, and another test can take it in between: a test that
// needs the redirect caught tries again on another (portTaken).
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// portTaken says catchRedirect couldn't listen because something had the port.
func portTaken(err error) bool {
	return err != nil && strings.Contains(err.Error(), "address already in use")
}

func TestCatchRedirectReturnsTheQueryOfItsPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for range 5 {
		if !catchesARedirect(ctx, t) {
			return
		}
	}
	t.Fatal("every port tried was taken")
}

// catchesARedirect checks catchRedirect on a free port, and says whether the port was taken first.
func catchesARedirect(ctx context.Context, t *testing.T) (taken bool) {
	t.Helper()
	redirect := fmt.Sprintf("http://localhost:%d/callback", freePort(t))
	base := strings.Replace(redirect, "localhost", "127.0.0.1", 1)
	got, err := catchRedirect(ctx, redirect, func() {
		go func() {
			// Another path isn't the redirect.
			if resp, err := http.Get(strings.TrimSuffix(base, "callback") + "favicon.ico"); err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusNotFound {
					t.Errorf("another path answered %s", resp.Status)
				}
			}
			resp, err := http.Get(base + "?code=abc&state=1")
			if err != nil {
				t.Error(err)
				return
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if !strings.Contains(string(body), "close this tab") {
				t.Errorf("the page says %q", body)
			}
		}()
	})
	if portTaken(err) {
		return true
	}
	if err != nil || got != "code=abc&state=1" {
		t.Fatalf("got %q, %v", got, err)
	}
	return false
}

func TestCatchRedirectRefusesAnotherMachineAndGivesUp(t *testing.T) {
	if _, err := catchRedirect(context.Background(), "http://example.com:80/cb", func() {}); err == nil {
		t.Fatal("listened for another machine")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	redirect := fmt.Sprintf("http://127.0.0.1:%d/", freePort(t))
	if _, err := catchRedirect(ctx, redirect, func() {}); err == nil {
		t.Fatal("waited past its context")
	}
}

func TestAskReadsALineAndSaysWhenThereIsNone(t *testing.T) {
	var stderr strings.Builder
	s := setupper{stderr: &stderr, in: bufio.NewReader(strings.NewReader("app-1\r\nlast"))}
	for _, want := range []string{"app-1", "last"} {
		got, err := s.ask(context.Background(), contractv1.SetupAsk{Prompt: "App ID", Secret: true}) // not a terminal
		if err != nil || got != want {
			t.Fatalf("got %q, %v, want %q", got, err, want)
		}
	}
	_, err := s.ask(context.Background(), contractv1.SetupAsk{Prompt: "App ID"})
	if err == nil || !strings.Contains(err.Error(), "stdin ended") {
		t.Fatalf("got %v, want stdin ended", err)
	}
	if stderr.String() != "App ID: App ID: App ID: " {
		t.Errorf("stderr %q", stderr.String())
	}
}

func TestAskStopsWhenItsContextEnds(t *testing.T) {
	never, _ := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := setupper{stderr: io.Discard, in: bufio.NewReader(never)}
	if _, err := s.ask(ctx, contractv1.SetupAsk{Prompt: "Code"}); err != context.Canceled {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

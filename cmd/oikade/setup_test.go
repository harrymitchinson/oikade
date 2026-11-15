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

// freePort is a port on 127.0.0.1 nothing listens on, for now.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func TestCatchRedirectReturnsTheQueryOfItsPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
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
	if err != nil || got != "code=abc&state=1" {
		t.Fatalf("got %q, %v", got, err)
	}
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
		got, err := s.ask(contractv1.SetupAsk{Prompt: "App ID", Secret: true}) // not a terminal, so read
		if err != nil || got != want {
			t.Fatalf("got %q, %v, want %q", got, err, want)
		}
	}
	_, err := s.ask(contractv1.SetupAsk{Prompt: "App ID"})
	if err == nil || !strings.Contains(err.Error(), "stdin ended") {
		t.Fatalf("got %v, want stdin ended", err)
	}
	if stderr.String() != "App ID: App ID: App ID: " {
		t.Errorf("stderr %q", stderr.String())
	}
}

package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dialer is a Transport whose host is a function of the connection.
type dialer func(ctx context.Context) (net.Conn, error)

func (d dialer) Dial(ctx context.Context) (net.Conn, error) { return d(ctx) }

// hangsUp reads a request, then closes the connection without answering.
func hangsUp(context.Context) (net.Conn, error) {
	client, host := net.Pipe()
	go func() {
		defer func() { _ = host.Close() }()
		_, _ = http.ReadRequest(bufio.NewReader(host))
	}()
	return client, nil
}

func TestCallSaysWhetherTheHostHadIt(t *testing.T) {
	ctx := context.Background()
	unreachable := dialer(func(context.Context) (net.Conn, error) { return nil, errors.New("no socket") })
	_, err := Call(ctx, unreachable, Request{Type: TypeStatus})
	if err == nil || !strings.HasPrefix(err.Error(), "reaching the host") {
		t.Errorf("a host that isn't there: %v", err)
	}
	_, err = Call(ctx, dialer(hangsUp), Request{Type: TypeDeploy})
	if err == nil || !strings.HasPrefix(err.Error(), "lost the host before it answered") {
		t.Errorf("a host that went away: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "http://") {
		t.Errorf("the made-up URL is in %q", err)
	}
}

func TestTailCancelledBeforeTheHostAnswersIsNoError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	waits := dialer(func(ctx context.Context) (net.Conn, error) {
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err := Tail(ctx, waits, func(Event) error { return nil }); err != nil {
		t.Errorf("a cancelled tail: %v", err)
	}
}

// streams reads a request, then answers it with body and closes the connection.
func streams(body string) dialer {
	return func(context.Context) (net.Conn, error) {
		client, host := net.Pipe()
		go func() {
			defer func() { _ = host.Close() }()
			if _, err := http.ReadRequest(bufio.NewReader(host)); err != nil {
				return
			}
			_, _ = io.WriteString(host, "HTTP/1.1 200 OK\r\nContent-Type: "+StreamContentType+
				"\r\nConnection: close\r\n\r\n"+body)
		}()
		return client, nil
	}
}

func TestTailSaysWhetherTheHostEndedIt(t *testing.T) {
	ctx := context.Background()
	var got []Event
	each := func(e Event) error {
		got = append(got, e)
		return nil
	}
	err := Tail(ctx, streams(`{}`+"\n"+`{"type":"change","device":"hall-lamp"}`+"\n"), each)
	if !errors.Is(err, ErrHostStopped) {
		t.Errorf("a stream the host ended: %v", err)
	}
	if len(got) != 1 || got[0].Device != "hall-lamp" {
		t.Errorf("events: %+v", got)
	}
	err = Tail(ctx, streams(`{}`+"\n"+`{"type":"cha`), each)
	if err == nil || errors.Is(err, ErrHostStopped) || !strings.HasPrefix(err.Error(), "lost the host") {
		t.Errorf("a stream cut off mid-line: %v", err)
	}
}

func TestADeployCutOffSaysNothingWasDeployed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home")
	if err := os.WriteFile(path, make([]byte, 4<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	// The host reads the request's head, then goes.
	_, err := Deploy(context.Background(), dialer(hangsUp), path, false, nil)
	if err == nil || !strings.Contains(err.Error(), "nothing was deployed") {
		t.Errorf("a deploy cut off mid-upload: %v", err)
	}
}

func TestAddressDefaultsThePort(t *testing.T) {
	for in, want := range map[string]string{
		"pi.home":      "pi.home:8443",
		"pi.home:9000": "pi.home:9000",
		":8443":        ":8443",
		"::1":          "[::1]:8443",
		"[::1]":        "[::1]:8443",
		"192.168.1.5":  "192.168.1.5:8443",
	} {
		if got := Address(in); got != want {
			t.Errorf("Address(%q) = %q, want %q", in, got, want)
		}
	}
}

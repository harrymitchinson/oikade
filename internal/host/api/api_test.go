package api

import (
	"bufio"
	"context"
	"errors"
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

func TestADeployCutOffSaysNothingWasDeployed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home")
	if err := os.WriteFile(path, make([]byte, 4<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	// The host reads the request's head, then goes.
	_, err := Deploy(context.Background(), dialer(hangsUp), path, false)
	if err == nil || !strings.Contains(err.Error(), "nothing was deployed") {
		t.Errorf("a deploy cut off mid-upload: %v", err)
	}
}

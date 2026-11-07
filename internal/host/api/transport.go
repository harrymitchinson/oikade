package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Transport reaches a host's API.
type Transport interface {
	// Dial opens a connection to the API.
	Dial(ctx context.Context) (net.Conn, error)
}

// Local is a host on this machine.
type Local struct {
	// DataDir is the host's data directory.
	DataDir string
}

// Dial implements Transport.
func (l Local) Dial(ctx context.Context) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(l.DataDir, SocketName))
}

// SSH is a host on another machine, reached with the user's own ssh and its config. The socket's
// permissions mean the SSH user must be the one running the host, and oikade must be on its PATH.
type SSH struct {
	// Host is what to give ssh: a name from the SSH config, or user@address.
	Host string
	// DataDir is the data directory on that machine.
	DataDir string
}

func (s SSH) command(ctx context.Context, remote string) (*exec.Cmd, error) {
	if s.Host == "" || strings.HasPrefix(s.Host, "-") {
		return nil, fmt.Errorf("bad host %q", s.Host)
	}
	cmd := exec.CommandContext(ctx, "ssh", "-T", "--", s.Host, remote)
	cmd.Stderr = os.Stderr
	return cmd, nil
}

// Dial implements Transport, with oikade proxy on the other end joining the session to the socket.
func (s SSH) Dial(ctx context.Context) (net.Conn, error) {
	// Not ctx: the HTTP client closes the connection when a call ends, and reads until then.
	cmd, err := s.command(context.WithoutCancel(ctx), "oikade proxy --data-dir "+Quote(s.DataDir))
	if err != nil {
		return nil, err
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("running ssh: %w", err)
	}
	return &piped{Reader: out, in: in, cmd: cmd}, nil
}

// piped is a connection through a child process.
type piped struct {
	io.Reader
	in   io.WriteCloser
	cmd  *exec.Cmd
	once sync.Once
}

func (p *piped) Write(b []byte) (int, error) { return p.in.Write(b) }

// A process has no addresses, and its pipes take no deadlines here: the HTTP client ends a call by
// closing the connection.
func (p *piped) LocalAddr() net.Addr              { return pipeAddr{} }
func (p *piped) RemoteAddr() net.Addr             { return pipeAddr{} }
func (p *piped) SetDeadline(time.Time) error      { return nil }
func (p *piped) SetReadDeadline(time.Time) error  { return nil }
func (p *piped) SetWriteDeadline(time.Time) error { return nil }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "ssh" }

func (p *piped) Close() error {
	p.once.Do(func() {
		_ = p.in.Close()
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Wait()
	})
	return nil
}

// Upload copies a file to a new private directory there, for oikade install. It returns the file's
// path there, and what removes it.
func (s SSH) Upload(ctx context.Context, path string) (string, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = f.Close() }()
	cmd, err := s.command(ctx, `dir=$(mktemp -d) && cat > "$dir/home" && echo "$dir"`)
	if err != nil {
		return "", nil, err
	}
	var out bytes.Buffer
	cmd.Stdin, cmd.Stdout = f, &out
	if err := cmd.Run(); err != nil {
		return "", nil, fmt.Errorf("copying the binary to %s: %w", s.Host, err)
	}
	dir := strings.TrimSpace(out.String())
	if dir == "" {
		return "", nil, fmt.Errorf("copying the binary to %s: no path came back", s.Host)
	}
	return dir + "/home", func() {
		if cmd, err := s.command(context.WithoutCancel(ctx), "rm -rf -- "+Quote(dir)); err == nil {
			_ = cmd.Run()
		}
	}, nil
}

// Output runs a command there and returns what it printed.
func (s SSH) Output(ctx context.Context, remote string) (string, error) {
	cmd, err := s.command(ctx, remote)
	if err != nil {
		return "", err
	}
	out, err := cmd.Output()
	return string(out), err
}

// Attached runs a command there on this terminal, so that sudo can ask for a password.
func (s SSH) Attached(ctx context.Context, remote string) error {
	cmd, err := s.command(ctx, remote)
	if err != nil {
		return err
	}
	cmd.Args[1] = "-t"
	cmd.Stdin, cmd.Stdout = os.Stdin, os.Stdout
	return cmd.Run()
}

// Quote makes s one word for a POSIX shell.
func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

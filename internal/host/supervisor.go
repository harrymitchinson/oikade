package host

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/wire"
)

const (
	// DefaultHeartbeatTimeout is how long a process may go without answering.
	DefaultHeartbeatTimeout = 10 * time.Second
	// firstBackoff and maxBackoff bound the wait before a restart. It doubles each time, and goes
	// back to firstBackoff once a process has stayed up for stableAfter.
	firstBackoff = time.Second
	maxBackoff   = 30 * time.Second
	stableAfter  = time.Minute
	// stopTimeout is how long a process gets to stop when the host does.
	stopTimeout = 5 * time.Second
)

// supervise keeps your process running until ctx ends. Only one runs at a time: each is waited for,
// and its session ended, before the next starts.
func (h *Host) supervise(ctx context.Context, srv *server, socket string) {
	backoff := firstBackoff
	for ctx.Err() == nil {
		started := time.Now()
		err := h.runProcess(ctx, srv, socket)
		srv.disconnect()
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= stableAfter {
			backoff = firstBackoff
		}
		h.Log.Info("Restarting automations process", "reason", reason(err), "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func reason(err error) string {
	if err == nil {
		return "exited"
	}
	return err.Error()
}

// errHeartbeat is a process that stopped answering.
var errHeartbeat = errors.New("stopped answering heartbeats")

// runProcess starts the process and returns when it has gone, having killed it if it stopped
// answering or ctx ended.
func (h *Host) runProcess(ctx context.Context, srv *server, socket string) error {
	cmd := exec.Command(h.Home, "automations")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), wire.SocketEnv+"="+socket)
	if err := cmd.Start(); err != nil {
		return err
	}
	h.Log.Info("Started automations process", "pid", cmd.Process.Pid)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	timeout := h.HeartbeatTimeout
	ticker := time.NewTicker(timeout / 5)
	defer ticker.Stop()
	// A process that never connects counts as one that stopped answering.
	alive := time.Now()
	var id uint64
	for {
		select {
		case err := <-exited:
			return err
		case <-ctx.Done():
			return h.stop(cmd, exited)
		case <-ticker.C:
			id++
			if last, ok := srv.ping(id); ok && last.After(alive) {
				alive = last
			}
			if time.Since(alive) < timeout {
				continue
			}
			h.Log.Info("Killing automations process; it stopped answering", "pid", cmd.Process.Pid, "timeout", timeout.String())
			_ = cmd.Process.Kill()
			if err := <-exited; err == nil {
				// It answered nothing, yet exited cleanly before the kill landed.
				return nil
			}
			return errHeartbeat
		}
	}
}

// stop asks the process to stop, and kills it if it doesn't.
func (h *Host) stop(cmd *exec.Cmd, exited <-chan error) error {
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-exited:
		return err
	case <-time.After(stopTimeout):
		_ = cmd.Process.Kill()
		return <-exited
	}
}

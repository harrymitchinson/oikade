package host

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/go-logr/logr"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
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

// process is one start of your binary.
type process struct {
	// gone closes once it has gone and its session has ended.
	gone chan struct{}
	err  error
}

// failure says why it went. Going at all counts, while a version is settling.
func (p *process) failure() error {
	if p.err != nil {
		return p.err
	}
	return errExited
}

// swap asks the supervisor to replace the binary it runs. started gets the new process, or nil if
// path is empty. vet, if set, runs once the old process has gone and its session has ended, and an
// error stops the new one starting: started gets a process that has already gone with that error, and
// the supervisor waits for the next swap.
type swap struct {
	path    string
	started chan *process
	vet     func() error
}

// supervise keeps your process running until ctx ends. Only one runs at a time: each is waited for,
// and its session ended, before the next starts. swaps replace the binary between them.
func (h *Host) supervise(ctx context.Context, srv *server, socket, path string, d *deployer) {
	backoff := firstBackoff
	var next *swap
	for ctx.Err() == nil {
		if path == "" {
			h.plugins.use("")
			if next != nil {
				next.started <- nil
			}
			select {
			case <-ctx.Done():
				return
			case next = <-d.swaps:
				path = next.path
				backoff = firstBackoff
			}
			continue
		}
		if next != nil && next.vet != nil {
			if err := next.vet(); err != nil {
				p := &process{gone: make(chan struct{}), err: err}
				close(p.gone)
				next.started <- p
				next, path = nil, ""
				continue
			}
		}
		// Only now is this binary the one that runs: a refused deploy leaves the plugins alone.
		h.plugins.use(path)
		// The old process's session has ended, so a session from here on is this process's.
		select {
		case <-d.connected:
		default:
		}
		p := &process{gone: make(chan struct{})}
		if next != nil {
			next.started <- p
			next = nil
		}
		started := time.Now()
		var err error
		next, err = h.runProcess(ctx, srv, socket, path, d.swaps)
		srv.disconnect()
		p.err = err
		close(p.gone)
		if ctx.Err() != nil {
			return
		}
		if next != nil {
			path, backoff = next.path, firstBackoff
			continue
		}
		if time.Since(started) >= stableAfter {
			backoff = firstBackoff
		}
		h.Log.Info("Restarting automations process", "reason", reason(err), "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		case next = <-d.swaps:
			path = next.path
			backoff = firstBackoff
			continue
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
// answering or ctx ended, or stopped it for a swap, which it returns.
func (h *Host) runProcess(ctx context.Context, srv *server, socket, path string, swaps <-chan *swap) (*swap, error) {
	cmd := exec.Command(path, wire.RoleAutomations)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), wire.SocketEnv+"="+socket, contractv1.EnvSecretsDir+"="+h.SecretsDir)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	h.Log.Info("Started automations process", "pid", cmd.Process.Pid)
	return h.watch(logr.NewContext(ctx, h.Log), cmd, "automations", srv.ping, swaps)
}

// watch waits for a started process, killing it if it stops answering pings, and stopping it when
// ctx ends or a swap arrives, which it returns. ping asks it to answer and says when it last did.
func (h *Host) watch(ctx context.Context, cmd *exec.Cmd, kind string,
	ping func(id uint64) (time.Time, bool), swaps <-chan *swap) (*swap, error) {
	log := logr.FromContextOrDiscard(ctx)
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
			return nil, err
		case <-ctx.Done():
			return nil, h.stop(cmd, exited)
		case sw := <-swaps:
			log.Info("Stopping "+kind+" process for a new version", "pid", cmd.Process.Pid)
			return sw, h.stop(cmd, exited)
		case <-ticker.C:
			id++
			if last, ok := ping(id); ok && last.After(alive) {
				alive = last
			}
			if time.Since(alive) < timeout {
				continue
			}
			log.Info("Killing "+kind+" process; it stopped answering", "pid", cmd.Process.Pid, "timeout", timeout.String())
			_ = cmd.Process.Kill()
			if err := <-exited; err == nil {
				// It answered nothing, yet exited cleanly before the kill landed.
				return nil, nil
			}
			return nil, errHeartbeat
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

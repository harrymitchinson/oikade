package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/host/wire"
)

const (
	// DefaultSettle is how long a new version must stay up before it counts.
	DefaultSettle = 5 * time.Second
	// checkTimeout bounds a binary's check mode, which only loads a home.
	checkTimeout = 30 * time.Second
	// checkOutput bounds what check mode's output is kept of.
	checkOutput = 4 << 20
)

// deployer replaces your binary. It runs one change at a time.
type deployer struct {
	mu       sync.Mutex
	log      logr.Logger
	versions *versions
	srv      *server
	settle   time.Duration
	// home is the binary from the host's flags, which runs until something is deployed.
	home string
	// running is the path of the binary the supervisor runs, or empty for none.
	running string
	// swaps hands the supervisor the next binary, and connected hears of each session.
	swaps     chan *swap
	connected chan struct{}
}

// startup returns the binary to run, and finishes a deploy the last host didn't. That version never
// proved itself, so the one before it comes back.
func (d *deployer) startup() string {
	if d.versions.state.Pending {
		failed := d.versions.state.Versions[0].ID
		if err := d.versions.abort(); err != nil {
			d.log.Error(err, "Failed to restore the last automations version", "version", failed)
		} else {
			d.log.Info("Restored the last automations version; a deploy didn't finish", "version", failed)
		}
	}
	d.running = d.home
	if v, ok := d.versions.current(); ok {
		d.running = d.versions.path(v.ID)
	}
	return d.running
}

// deploy takes a new binary. Nothing stops until it has passed its check.
func (d *deployer) deploy(ctx context.Context, binary string, allow bool) api.Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.adopt(ctx); err != nil {
		return api.Response{Error: err.Error()}
	}
	id, err := d.versions.add(binary)
	if err != nil {
		return api.Response{Error: fmt.Sprintf("storing the binary: %s", err)}
	}
	if cur, ok := d.versions.current(); ok && cur.ID == id {
		return api.Response{Version: id, Unchanged: true}
	}
	return d.takeOver(ctx, id, allow)
}

// rollback returns to the version before the running one, on the same terms as a deploy.
func (d *deployer) rollback(ctx context.Context, allow bool) api.Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	prev, ok := d.versions.previous()
	if !ok {
		return api.Response{Error: "there's no earlier version"}
	}
	return d.takeOver(ctx, prev.ID, allow)
}

// adopt records a binary from the host's flags, so a deploy can go back to it. A binary too old to
// have a check mode still counts, with nothing known about it.
func (d *deployer) adopt(ctx context.Context) error {
	if _, ok := d.versions.current(); ok || d.home == "" {
		return nil
	}
	id, err := d.versions.add(d.home)
	if err != nil {
		return fmt.Errorf("storing the running binary: %w", err)
	}
	manifest, err := runCheck(ctx, d.versions.path(id))
	if err != nil {
		d.log.Info("Couldn't check the running binary", "reason", err.Error())
	}
	return d.versions.adopt(version{ID: id, Manifest: manifest, At: time.Now()})
}

// takeOver checks a stored binary, stops what's running, starts it and watches it settle. If it fails
// to, the last version comes back.
func (d *deployer) takeOver(ctx context.Context, id string, allow bool) api.Response {
	reject := func(resp api.Response) api.Response {
		if err := d.versions.discard(id); err != nil {
			d.log.Error(err, "Failed to remove a rejected automations version", "version", id)
		}
		d.log.Info("Rejected automations version", "version", id, "reason", resp.Error)
		return resp
	}
	manifest, err := runCheck(ctx, d.versions.path(id))
	if err != nil {
		return reject(api.Response{Error: err.Error()})
	}
	var old wire.Manifest
	if cur, ok := d.versions.current(); ok {
		old = cur.Manifest
	}
	held := d.srv.engine.RuleClaims(wire.Namespace)
	if findings := safetyFindings(old, manifest, held); len(findings) > 0 && !allow {
		return reject(api.Response{Error: "this would remove Safety rules", Findings: findings})
	}

	last := d.running
	if err := d.versions.begin(version{ID: id, Manifest: manifest, At: time.Now()}); err != nil {
		return reject(api.Response{Error: fmt.Sprintf("recording the version: %s", err)})
	}
	d.srv.settling.Store(true)
	defer d.srv.settling.Store(false)
	err = d.settled(ctx, d.versions.path(id))
	if ctx.Err() != nil {
		// The next host works out what to do from the record.
		return api.Response{Error: "the host stopped before the version settled"}
	}
	if err != nil {
		return d.restore(ctx, id, last, err)
	}
	if err := d.versions.commit(); err != nil {
		d.log.Error(err, "Failed to confirm automations version", "version", id)
		return api.Response{Error: fmt.Sprintf("confirming the version: %s", err), Version: id}
	}
	d.srv.settling.Store(false)
	d.srv.reconcile()
	d.log.Info("Deployed automations version", "version", id)
	return api.Response{Version: id}
}

// restore brings the last version back after a new one failed.
func (d *deployer) restore(ctx context.Context, failed, last string, cause error) api.Response {
	d.log.Info("Restoring the last automations version; a new one failed", "version", failed, "reason", cause.Error())
	if err := d.versions.abort(); err != nil {
		d.log.Error(err, "Failed to record the failed automations version", "version", failed)
	}
	resp := api.Response{Error: fmt.Sprintf("the new version failed: %s", cause), Restored: true}
	if cur, ok := d.versions.current(); ok {
		resp.Version = cur.ID
	}
	if err := d.settled(ctx, last); err != nil {
		resp.Restored = false
		resp.Error += fmt.Sprintf("; the last version failed too: %s", err)
		d.log.Info("Failed to restore the last automations version", "reason", err.Error())
		return resp
	}
	d.log.Info("Restored the last automations version", "version", resp.Version)
	return resp
}

// settled hands the supervisor a binary, and returns once it has connected and stayed up.
func (d *deployer) settled(ctx context.Context, path string) error {
	sw := &swap{path: path, started: make(chan *process, 1)}
	select {
	case d.swaps <- sw:
	case <-ctx.Done():
		return ctx.Err()
	}
	d.running = path
	var p *process
	select {
	case p = <-sw.started:
	case <-ctx.Done():
		return ctx.Err()
	}
	if p == nil {
		return nil // Nothing to run.
	}
	select {
	case <-p.gone:
		return p.failure()
	case <-ctx.Done():
		return ctx.Err()
	case <-d.connected:
	}
	timer := time.NewTimer(d.settle)
	defer timer.Stop()
	select {
	case <-p.gone:
		return p.failure()
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// safetyFindings lists what a new version would lose. A Safety rule's claims end with it, which could
// reopen a valve, and a latch dies with its name, so a rule renamed is one removed. held catches a
// Safety claim the rule's own declaration doesn't show, such as one a flow makes in a step.
func safetyFindings(old, next wire.Manifest, held map[string]control.Level) []string {
	stays := func(name string) bool {
		return slices.ContainsFunc(next.Rules, func(r wire.RuleInfo) bool { return r.Name == name })
	}
	var findings []string
	named := map[string]bool{}
	for _, r := range old.Rules {
		if !r.Safety || stays(r.Name) {
			continue
		}
		named[r.Name] = true
		finding := fmt.Sprintf("Safety %s %q is gone", r.Kind, r.Name)
		if r.Latch {
			finding += ", and its latch would be lost if it was renamed"
		}
		findings = append(findings, finding)
	}
	for rule, level := range held {
		if level >= control.Safety && !named[rule] && !stays(rule) {
			findings = append(findings, fmt.Sprintf("rule %q is gone and holds a Safety claim, which would end", rule))
		}
	}
	slices.Sort(findings)
	return findings
}

// runCheck runs a binary's check mode and returns what it says about itself. A binary that fails it,
// or was built for another protocol, goes no further.
func runCheck(ctx context.Context, path string) (wire.Manifest, error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, wire.RoleCheck)
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &limited{&stdout, checkOutput}, &limited{&stderr, checkOutput}
	if err := cmd.Run(); err != nil {
		if out := strings.TrimSpace(stderr.String()); out != "" {
			return wire.Manifest{}, fmt.Errorf("check failed: %w: %s", err, out)
		}
		return wire.Manifest{}, fmt.Errorf("check failed: %w", err)
	}
	// A home's own code may have printed first.
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	var m wire.Manifest
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &m); err != nil {
		return wire.Manifest{}, fmt.Errorf("check printed no manifest: %w", err)
	}
	if m.Protocol != wire.Version {
		return wire.Manifest{}, fmt.Errorf("this binary speaks protocol %d and the host speaks %d: "+
			"build it with this version of oikade", m.Protocol, wire.Version)
	}
	return m, nil
}

// limited drops what's past its limit, and tells the writer it took it all.
type limited struct {
	b *bytes.Buffer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.n - l.b.Len(); room > 0 {
		l.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

var errExited = errors.New("it exited")

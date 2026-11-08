package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/fault"
	"github.com/harrymitchinson/oikade/internal/flow"
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
	log      *slog.Logger
	versions *versions
	srv      *server
	settle   time.Duration
	// home is the binary from the host's flags, which runs until something is deployed.
	home string
	// running is the path of the binary the supervisor runs, or empty for none.
	running string
	// confirmable is a version that settled and couldn't be recorded as confirmed. Being unconfirmed
	// doesn't say that on its own: a version the host stopped watching is unconfirmed too.
	confirmable string
	// swaps hands the supervisor the next binary, and connected hears of each session.
	swaps     chan *swap
	connected chan struct{}
}

// startup returns the binary to run, and finishes a deploy the last host didn't. That version never
// proved itself, so the one before it comes back.
func (d *deployer) startup() string {
	if d.versions.state.Pending {
		failed := d.versions.state.Versions[0].ID
		if err := d.restoreRuns(failed); err != nil {
			d.log.Error("Failed to restore the flow runs of the last automations version", "err", err, "version", failed)
		}
		if err := d.versions.abort(); err != nil {
			d.log.Error("Failed to restore the last automations version", "err", err, "version", failed)
		} else {
			d.log.Info("Restored the last automations version; a deploy didn't finish", "version", failed)
		}
	}
	d.srv.resetHolds(d.versions.state.Released)
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
	return d.withUnimported(d.deployLocked(ctx, binary, allow))
}

func (d *deployer) deployLocked(ctx context.Context, binary string, allow bool) api.Response {
	id, err := d.versions.take(binary)
	if err != nil {
		return api.Response{Error: fmt.Sprintf("storing the binary: %s", err)}
	}
	if cur, ok := d.versions.current(); ok && cur.ID == id {
		if d.confirmable == id {
			return d.confirm(id)
		}
		if d.versions.state.Pending {
			return api.Response{Error: "this version hasn't settled", Version: id}
		}
		return d.unchanged(id)
	}
	return d.takeOver(ctx, id, allow, d.versions.begin)
}

// unchanged answers a deploy of the version already running.
func (d *deployer) unchanged(id string) api.Response {
	if err := d.removals(); err != nil {
		return api.Response{Error: removalsFailed(err), Version: id}
	}
	return api.Response{Version: id, Unchanged: true}
}

// rollback returns to the version before the running one, on the same terms as a deploy. The running
// one is forgotten, so a second rollback goes back further.
func (d *deployer) rollback(ctx context.Context, allow bool) api.Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	prev, ok := d.versions.previous()
	if !ok {
		return api.Response{Error: "there's no earlier version"}
	}
	return d.withUnimported(d.takeOver(ctx, prev.ID, allow, d.versions.beginRollback))
}

// withUnimported tells a deploy that went ahead of the devices left behind by plugins its version
// doesn't import. They stay on record, so that a plugin briefly missing loses nothing, until you
// remove them.
func (d *deployer) withUnimported(resp api.Response) api.Response {
	if resp.Error != "" || d.srv == nil || d.srv.plugins == nil {
		return resp
	}
	if cur, ok := d.versions.current(); ok && cur.Manifest.Protocol != 0 {
		resp.Unimported = d.srv.plugins.unimported(cur.Manifest.Plugins)
	}
	return resp
}

// keepHome stores and checks a binary from the host's flags, so a deploy can go back to it, and
// returns it, or nil if there's none to keep. It isn't on record until the deploy goes ahead: a
// refused deploy changes nothing, and a host restarted after one still runs what its flags say. One
// that can't be read or has no check mode costs that, which takeOver makes you agree to, because the
// deploy may be there to fix it.
func (d *deployer) keepHome(ctx context.Context) *version {
	if _, ok := d.versions.current(); ok || d.home == "" {
		return nil
	}
	id, err := d.versions.add(d.home)
	if err != nil {
		d.log.Info("Couldn't keep the running binary; a deploy can't go back to it", "reason", err.Error())
		return nil
	}
	manifest, err := runCheck(ctx, d.versions.path(id))
	if err != nil {
		d.log.Info("Couldn't check the running binary", "reason", err.Error())
	}
	return &version{ID: id, Manifest: manifest, At: time.Now()}
}

// dropHome removes what keepHome stored, if the deploy it was for was refused.
func (d *deployer) dropHome(home *version) {
	if home == nil {
		return
	}
	if err := d.versions.discard(home.ID); err != nil {
		d.log.Error("Failed to remove the copy of the running binary", "err", err, "version", home.ID)
	}
}

// removals ends what's left of rules the running version doesn't have, once a deploy has settled
// either way, sparing what a failed deploy found there. It's recorded, so a crash first finishes the
// job on the next start. It does nothing for an unconfirmed version, or one whose rules aren't known,
// which would look like a version with none, so that stays until the next confirmed one.
func (d *deployer) removals() error {
	cur, ok := d.versions.current()
	if !ok || !d.versions.state.Removals || d.versions.state.Pending || cur.Manifest.Protocol == 0 {
		return nil
	}
	rules, flows := slices.Clone(d.versions.state.Spared), slices.Clone(d.versions.state.Spared)
	for _, r := range cur.Manifest.Rules {
		rules = append(rules, r.Name)
		if r.Kind == wire.KindFlow {
			flows = append(flows, r.Name)
		}
	}
	if err := d.srv.endReleased(); err != nil {
		d.log.Error("Failed to end the claims of flow runs; the next start will", "err", err)
		return err
	}
	if err := d.srv.reconcile(rules, flows); err != nil {
		d.log.Error("Failed to end what removed rules left; the next start will", "err", err)
		return err
	}
	if err := d.srv.confirmSettings(cur.Manifest.Control); err != nil {
		d.log.Error("Failed to drop removed device settings; the next start will", "err", err)
		return err
	}
	if err := d.versions.removalsDone(); err != nil {
		d.log.Error("Failed to record that removed rules have gone", "err", err, "version", cur.ID)
	}
	return nil
}

func removalsFailed(err error) string {
	return fmt.Sprintf("the version is running, but removed rules left claims or state, which the next deploy or host start will retry: %s", err)
}

// takeOver checks a stored binary, stops what's running, starts it and watches it settle. If it fails
// to, the last version comes back.
func (d *deployer) takeOver(ctx context.Context, id string, allow bool, begin func(version, []string) error) api.Response {
	var home *version
	reject := func(resp api.Response) api.Response {
		d.dropHome(home)
		if err := d.versions.discard(id); err != nil {
			d.log.Error("Failed to remove a rejected automations version", "err", err, "version", id)
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
	} else if home = d.keepHome(ctx); home != nil {
		if home.ID == id {
			// It's the binary already running, and it has just passed its check.
			if err := d.versions.adopt(*home); err != nil {
				d.log.Error("Failed to record the running binary", "err", err)
			}
			return d.unchanged(id)
		}
		old = home.Manifest
		begin = func(next version, spared []string) error { return d.versions.beginFromHome(*home, next, spared) }
	}
	findings := safetyFindings(old, manifest, d.srv.engine.RuleClaims(), d.srv.safetyHeld)
	if d.running != "" && old.Protocol == 0 {
		findings = append(findings, "the running version couldn't be kept or checked, so a Safety rule it has can't be seen, and a failed deploy has nothing to go back to")
	}
	if len(findings) > 0 && !allow {
		return reject(api.Response{Error: "this would remove Safety rules", Findings: findings})
	}

	d.log.Info("Checked automations version", "version", id)
	fault.Hit(fault.HostDeployChecked)
	last := d.running
	spared, err := d.srv.leftovers()
	if err != nil {
		return reject(api.Response{Error: fmt.Sprintf("reading what's on record: %s", err)})
	}
	if err := d.srv.journal.DiscardCopy(); err != nil {
		return reject(api.Response{Error: fmt.Sprintf("clearing the last copy of the flow runs: %s", err)})
	}
	d.confirmable = ""
	if err := begin(version{ID: id, Manifest: manifest, At: time.Now()}, spared); err != nil {
		return reject(api.Response{Error: fmt.Sprintf("recording the version: %s", err)})
	}
	fault.Hit(fault.HostDeployBegun)
	// The old process could claim until it stopped, so the claims need reading again by then.
	var late []string
	vet := func() error {
		if !allow {
			late = safetyFindings(old, manifest, d.srv.engine.RuleClaims(), d.srv.safetyHeld)
			if len(late) > 0 {
				return errLateClaim
			}
		}
		// The old process has gone, so this is everything it left, for a failed version to give back.
		if err := d.srv.journal.Copy(); err != nil {
			return fmt.Errorf("copying the flow runs: %w", err)
		}
		d.srv.startHolding()
		return nil
	}
	err = d.settled(ctx, d.versions.path(id), vet)
	if ctx.Err() != nil {
		// The next host works out what to do from the record.
		return api.Response{Error: "the host stopped before the version settled"}
	}
	if errors.Is(err, errLateClaim) {
		resp := d.restore(ctx, id, last, err)
		resp.Findings = late
		return resp
	}
	if err != nil {
		return d.restore(ctx, id, last, fmt.Errorf("the new version failed: %w", err))
	}
	fault.Hit(fault.HostDeploySettled)
	return d.confirm(id)
}

// confirm makes a version that has settled the running one for good.
func (d *deployer) confirm(id string) api.Response {
	if err := d.versions.commit(d.srv.closeHolds()...); err != nil {
		d.log.Error("Failed to confirm automations version", "err", err, "version", id)
		d.srv.reopenHolds()
		d.confirmable = id
		return api.Response{Error: fmt.Sprintf("confirming the version, which deploying it again will retry: %s", err), Version: id}
	}
	d.confirmable = ""
	if err := d.srv.journal.DiscardCopy(); err != nil {
		d.log.Error("Failed to remove the copy of the flow runs; the next deploy will", "err", err)
	}
	if err := d.versions.prune(); err != nil {
		d.log.Error("Failed to remove old automations versions", "err", err)
	}
	fault.Hit(fault.HostDeployConfirmed)
	d.log.Info("Deployed automations version", "version", id)
	d.srv.emit(api.Event{Time: time.Now(), Type: api.EventDeploy, Message: id})
	if err := d.removals(); err != nil {
		return api.Response{Error: removalsFailed(err), Version: id}
	}
	return api.Response{Version: id}
}

// restore brings the last version back after a new one failed. With nothing before it, all there is
// to do is stop the failed one.
func (d *deployer) restore(ctx context.Context, failed, last string, cause error) api.Response {
	restoreErr := d.restoreRuns(failed)
	if restoreErr != nil {
		d.log.Error("Failed to restore the flow runs of the last automations version", "err", restoreErr, "version", failed)
	}
	if err := d.versions.abort(); err != nil {
		d.log.Error("Failed to record the failed automations version", "err", err, "version", failed)
	}
	if last == "" {
		if err := d.settled(ctx, "", nil); err != nil {
			d.log.Error("Failed to stop the failed automations version", "err", err, "version", failed)
		} else {
			d.log.Info("Stopped failed automations version; nothing ran before it", "version", failed, "reason", cause.Error())
		}
		return api.Response{Error: cause.Error()}
	}
	d.log.Info("Restoring the last automations version; a new one failed", "version", failed, "reason", cause.Error())
	resp := api.Response{Error: cause.Error(), Restored: true}
	if restoreErr != nil {
		resp.Error += fmt.Sprintf("; the flow runs it changed couldn't be put back: %s", restoreErr)
	}
	if cur, ok := d.versions.current(); ok {
		resp.Version = cur.ID
	}
	if err := d.settled(ctx, last, nil); err != nil {
		resp.Restored = false
		resp.Error += fmt.Sprintf("; the last version failed too: %s", err)
		d.log.Info("Failed to restore the last automations version", "reason", err.Error())
		return resp
	}
	d.log.Info("Restored the last automations version", "version", resp.Version)
	d.srv.emit(api.Event{Time: time.Now(), Type: api.EventDeploy, Message: resp.Version})
	if err := d.removals(); err != nil {
		resp.Error += "; " + removalsFailed(err)
	}
	return resp
}

// restoreRuns gives back the flow runs a failed version changed, and forgets the claims it released,
// which never ended. Its process has gone by now. Call it before the version is aborted, so that a
// crash leaves the deploy unfinished and the next start does this.
func (d *deployer) restoreRuns(failed string) error {
	d.srv.resetHolds(d.versions.state.Released)
	restored, err := d.srv.journal.Restore(flow.FlowOf)
	if err != nil {
		return err
	}
	if restored {
		d.log.Info("Restored the flow runs of the last automations version", "version", failed)
	}
	return nil
}

// settled hands the supervisor a binary, and returns once it has connected and stayed up.
func (d *deployer) settled(ctx context.Context, path string, vet func() error) error {
	sw := &swap{path: path, started: make(chan *process, 1), vet: vet}
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

// latest returns the running version, unless a deploy is under way and has it in flux.
func (d *deployer) latest() (version, bool) {
	if !d.mu.TryLock() {
		return version{}, false
	}
	defer d.mu.Unlock()
	return d.versions.current()
}

// safetyFindings lists what a new version would lose. A Safety rule's claims end with it, which could
// reopen a valve, and a latch dies with its name, so a rule renamed is one removed. held catches a
// Safety claim the rule's own declaration doesn't show, such as one a flow makes in a step.
func safetyFindings(old, next wire.Manifest, held map[string]control.Level, safetyHeld func(device, capability string) bool) []string {
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
	// A device stops being arbitrated with its Control, and that ends every claim on it.
	for _, c := range old.Control {
		kept := slices.ContainsFunc(next.Control, func(n wire.DeviceControl) bool {
			return n.Device == c.Device && n.Entry.Capability == c.Entry.Capability
		})
		if !kept && safetyHeld(c.Device, c.Entry.Capability) {
			findings = append(findings, fmt.Sprintf("%s on %s has no Control any more, which would end a Safety claim on it", c.Entry.Capability, c.Device))
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

// errLateClaim is a Safety claim the old version made after the check, so the check no longer holds.
var errLateClaim = errors.New("a rule this version doesn't have made a Safety claim while the old version stopped")

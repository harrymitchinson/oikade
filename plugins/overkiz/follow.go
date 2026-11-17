package overkiz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/sdk/plugin"
)

// reporter is what a cloud reports its devices to: the plugin, or its share of the plugin when a
// Cozytouch account follows two clouds.
type reporter interface {
	SetDiscovered(ctx context.Context, discovered []contractv1.DiscoveredDevice) error
	SetDevices(ctx context.Context, devices []contractv1.DeviceReport, undeclared int) error
	SetAvailability(ctx context.Context, id string, online bool) error
	ReportState(ctx context.Context, id string, s contractv1.State) error
	SetError(ctx context.Context, problem string) error
}

var _ reporter = (*plugin.Plugin)(nil)

// link is one cloud's or hub's side of following an account: a session, and what to do when it
// ends.
type link interface {
	// session logs in, reports the devices and follows them until something fails. connected runs
	// once they're reported.
	session(ctx context.Context, credential string, connected func()) error
	// pause drops the session and leaves the devices online, while it logs in again.
	pause()
	// lost ends the session: every device is offline, and the app can't be seen.
	lost(ctx context.Context)
	// absent reports that the account has nothing on this cloud.
	absent(ctx context.Context)
}

// follower keeps a link's sessions going: it reads the secret, logs in, and after a failure says
// why and waits as the failure asks.
type follower struct {
	account Account
	env     plugin.Environment
	timing  timing
	log     *slog.Logger
	out     reporter
	// name is the cloud or hub's API as a log names it: Overkiz or Magellan.
	name string
}

// follow keeps l's sessions going until ctx ends.
func (f *follower) follow(ctx context.Context, l link) {
	// wait is the backoff after a failure, and ended after sessions that end as they begin, which a
	// login doesn't reset.
	wait, ended := f.timing.retry, f.timing.retry
	for ctx.Err() == nil {
		credential, ok, err := plugin.ReadSecret(f.env, f.account.secret())
		switch {
		case err != nil:
			f.setProblem(ctx, fmt.Sprintf("could not read the secret %q: %v", f.account.secret(), err))
			f.untilSecretChanges(ctx, "", f.timing.refused)
			continue
		case !ok || strings.TrimSpace(credential) == "":
			f.setProblem(ctx, fmt.Sprintf("waiting for the secret %s", f.account.secret()))
			f.untilSecretChanges(ctx, "", f.timing.refused)
			continue
		}
		credential = strings.TrimSpace(credential)
		var live time.Time
		err = l.session(ctx, credential, func() { wait, live = f.timing.retry, time.Now() })
		if ctx.Err() != nil {
			break
		}
		// A cloud session ends by itself, and one that lasted logs in again at once, its devices
		// online meanwhile. A credential refused then is refused for real. One that ends as soon as
		// it starts backs off, so a cloud that keeps ending them isn't asked to log in over and over.
		expired := f.account.expires() && !live.IsZero() && !errors.Is(err, errReport) && classify(err) == refused
		if expired && time.Since(live) >= f.timing.retryMax {
			f.log.Info(f.name+" ended the plugin's session, so it's logging in again", "err", err, "account", f.account.describe())
			ended = f.timing.retry
			// Its devices stay online meanwhile, and a write fails at once rather than on a dead session.
			l.pause()
			continue
		}
		// A cloud still without an account for the login, asked again, had nothing to lose.
		if classify(err) != absent || !live.IsZero() {
			l.lost(ctx)
		}
		switch {
		case expired:
			f.log.Warn(f.name+" ended the plugin's session as it began", "err", err, "account", f.account.describe(), "retry", ended.String())
			f.setProblem(ctx, fmt.Sprintf("%s keeps ending the plugin's session: trying again in %s", f.account.describe(), ended))
			f.backoff(ctx, ended)
			ended = min(2*ended, f.timing.retryMax)
		case errors.Is(err, errReport):
			f.log.Error("Could not report "+f.name+" devices", "err", err, "account", f.account.describe(), "retry", wait.String())
			f.setProblem(ctx, fmt.Sprintf("could not report the devices of %s: %v", f.account.describe(), err))
			f.backoff(ctx, wait)
			wait = min(2*wait, f.timing.retryMax)
		case classify(err) == absent:
			// Not a problem: the account's devices are on its other cloud. It may gain some here, so
			// it's asked again hourly.
			f.log.Info("Found no "+f.name+" account for the login", "err", err, "account", f.account.describe())
			l.absent(ctx)
			f.setProblem(ctx, "")
			sleep(ctx, f.timing.refused)
		case classify(err) == refused:
			f.log.Warn(f.name+" refused the credential", "err", err, "account", f.account.describe())
			f.setProblem(ctx, fmt.Sprintf("%s refused the login (%v): check the secret %q", f.account.describe(), err, f.account.secret()))
			f.untilSecretChanges(ctx, credential, f.timing.refused)
		case classify(err) == limited:
			f.log.Warn(f.name+" limited the plugin's requests", "err", err, "account", f.account.describe(), "retry", f.timing.limited.String())
			f.setProblem(ctx, fmt.Sprintf("%s says there were too many requests: trying again in %s", f.account.describe(), f.timing.limited))
			f.backoff(ctx, f.timing.limited)
		case certFailure(err) != nil:
			f.log.Warn("Refused "+f.name+" hub's certificate", "err", err, "account", f.account.describe(), "retry", wait.String())
			f.setProblem(ctx, fmt.Sprintf("%s has a certificate that %v", f.account.describe(), certFailure(err)))
			f.backoff(ctx, wait)
			wait = min(2*wait, f.timing.retryMax)
		default:
			f.log.Warn("Could not reach "+f.name, "err", err, "account", f.account.describe(), "retry", wait.String())
			f.setProblem(ctx, fmt.Sprintf("could not reach %s: %v", f.account.describe(), err))
			f.backoff(ctx, wait)
			wait = min(2*wait, f.timing.retryMax)
		}
	}
}

func (f *follower) check(err error, msg, id string) {
	if err != nil && !errors.Is(err, plugin.ErrClosed) && !errors.Is(err, context.Canceled) {
		f.log.Error(msg, "err", err, "device", id)
	}
}

// setProblem sets the plugin's error, which oikade status shows.
func (f *follower) setProblem(ctx context.Context, problem string) {
	f.check(f.out.SetError(ctx, problem), "Could not report the plugin's problem", "")
}

// untilSecretChanges waits until the secret is other than was, or for at most most.
func (f *follower) untilSecretChanges(ctx context.Context, was string, most time.Duration) {
	deadline := time.Now().Add(most)
	for time.Now().Before(deadline) {
		if !sleep(ctx, f.timing.secret) {
			return
		}
		now, _, _ := plugin.ReadSecret(f.env, f.account.secret())
		if strings.TrimSpace(now) != was {
			return
		}
	}
}

// backoff waits d after a failure.
func (f *follower) backoff(ctx context.Context, d time.Duration) {
	if f.timing.backoff != nil {
		f.timing.backoff(ctx, d)
		return
	}
	sleep(ctx, d)
}

// errReport is a report the SDK refused or couldn't publish: the plugin's fault, or the host's
// connection, not the hub's.
var errReport = errors.New("could not report to Oikade")

// reporting marks err as errReport.
func reporting(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", errReport, err)
}

// sleep waits for d, and says false if ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

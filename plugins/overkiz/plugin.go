package overkiz

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/sdk/plugin"
)

// Account is where the plugin reaches Overkiz. Local is a TaHoma hub's local API.
type Account interface {
	// secret is the name of the secret that holds the credential, or "" if it has none.
	secret() string
	// secretField is the field that holds it, as an error names it.
	secretField() string
	// connect returns a client authorized with the credential, logging in first if it must.
	connect(ctx context.Context, credential string) (*client, error)
	// timing is how often to poll, and how long to wait.
	timing() timing
	// describe is the account as a log or an error says it.
	describe() string
	// check says what's wrong with its configuration.
	check() error
	// manufacturer is who makes its devices, for those that don't say.
	manufacturer() string
	// expires is whether its sessions end by themselves, as the cloud's do. A hub's token doesn't.
	expires() bool
}

// Local is a TaHoma hub's local API, which its Developer Mode turns on. The first-generation square
// TaHoma and the Connectivity Kit don't have it.
type Local struct {
	// Address is the hub's host and port, such as 192.168.1.40:8443 or
	// gateway-1234-5678-9012.local:8443.
	Address string
	// Token is the token you made in Developer Mode, such as oikade.Secret("tahoma-token").
	Token plugin.Secret
	// PIN is the hub's PIN, such as 1234-5678-9012, which its certificate is checked against when
	// Address isn't of the form gateway-<pin>.local, which gives it already. Without one, any
	// certificate Overkiz signed for a hub passes.
	PIN string
	// SkipCertificateCheck trusts whatever answers at Address, for a hub whose real certificate the
	// check refuses. Anything on the network that poses as the hub then gets its token.
	SkipCertificateCheck bool

	// roots is the tests' authority, in place of Overkiz's.
	roots *x509.CertPool
}

// pin is the PIN the hub's certificate is checked against, or "" if it isn't known.
func (l Local) pin() string { return cmp.Or(gatewayPIN(l.Address), l.PIN) }

func (l Local) secret() string { return l.Token.Name() }

func (Local) secretField() string { return "overkiz.Local's Token" }

func (l Local) describe() string { return "the TaHoma hub at " + l.Address }

func (l Local) timing() timing { return localTiming }

func (l Local) manufacturer() string { return "" }

func (l Local) expires() bool { return false }

func (l Local) check() error {
	named := gatewayPIN(l.Address)
	switch {
	case l.Address == "":
		return errors.New("overkiz.Local has no Address")
	case l.PIN != "" && !pinPattern.MatchString(l.PIN):
		return fmt.Errorf("overkiz.Local has PIN %q: it's the hub's PIN, such as 1234-5678-9012", l.PIN)
	case l.PIN != "" && named != "" && !strings.EqualFold(l.PIN, named):
		return fmt.Errorf("overkiz.Local has PIN %q, but its Address %q has the PIN %q", l.PIN, l.Address, named)
	}
	return nil
}

// warn logs how weak the certificate check is, if it is.
func (l Local) warn(log *slog.Logger) {
	switch {
	case l.SkipCertificateCheck:
		log.Warn("Skipping TaHoma hub's certificate check: anything posing as the hub gets its token", "account", l.describe())
	case l.pin() == "":
		log.Warn("Checking only that Overkiz signed TaHoma hub's certificate: set PIN to check it's this hub's", "account", l.describe())
	}
}

// connect needs no login: every request carries the token. The hub's certificate is signed by
// Overkiz's own root CA for its gateway-<pin>.local name, which hubTLS checks.
func (l Local) connect(_ context.Context, token string) (*client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = hubTLS(cmp.Or(l.roots, overkizRoots), l.pin())
	if l.SkipCertificateCheck {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // The user chose to.
	}
	return &client{
		http: &http.Client{Transport: transport, Timeout: localTiming.request},
		base: "https://" + l.Address + "/enduser-mobile-web/1/enduserAPI",
		authorize: func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
		},
	}, nil
}

// timing is how often the plugin polls and how long it waits, which tests shorten.
type timing struct {
	// poll is how often events are fetched, and busy how often while an execution runs.
	poll, busy time.Duration
	// retry is the first wait after the hub or cloud doesn't answer, doubling up to retryMax.
	retry, retryMax time.Duration
	// limited is how long to wait after being told there were too many requests.
	limited time.Duration
	// refused is how long to wait before trying refused credentials again, unless they change.
	refused time.Duration
	// secret is how often the secret is read while waiting for it to change.
	secret time.Duration
	// request bounds one request, and set a write, which must answer within the host's Matter
	// write timeout (5s).
	request, set time.Duration
	// execution is how long an execution counts as running without an event that ends it.
	execution time.Duration
	// backoff waits d after a failure, or until ctx ends. Nil is sleep; a test sees the waits
	// through it.
	backoff func(ctx context.Context, d time.Duration)
}

var localTiming = timing{
	poll: 5 * time.Second, busy: 2 * time.Second, retry: 5 * time.Second, retryMax: time.Minute,
	limited: time.Minute, refused: time.Hour, secret: 5 * time.Second,
	request: 10 * time.Second, set: 4 * time.Second, execution: 2 * time.Minute,
}

// New returns the plugin's Factory, for home.Plugin.
func New(a Account) plugin.Factory {
	return func(env plugin.Environment, declared []contractv1.DeclaredDevice) (plugin.Handler, error) {
		if a == nil {
			return nil, errors.New("overkiz.New has no Account")
		}
		if err := a.check(); err != nil {
			return nil, err
		}
		if a.secret() == "" {
			return nil, fmt.Errorf("%s isn't set: pass it an oikade.Secret", a.secretField())
		}
		log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("logger", "overkiz")
		if l, ok := a.(Local); ok {
			l.warn(log)
		}
		return newBridge(a, env, declared, a.timing(), log), nil
	}
}

// device is one Overkiz device the plugin serves, with its sub-devices folded in.
type device struct {
	id    string
	url   string
	model model
	// subs are the sub-devices' URLs, by sub, "" being the device itself.
	subs map[string]string
	info contractv1.DiscoveredDevice
	// available and enabled are what the setup or the last event said. It's online when both are.
	available, enabled bool
	// raw is every state it and its sub-devices reported, and values what the model reads from them.
	raw    states
	values contractv1.State
}

// bridge is the plugin.
type bridge struct {
	account  Account
	env      plugin.Environment
	declared []string
	timing   timing
	log      *slog.Logger

	plugin *plugin.Plugin
	// kick asks for an early fetch, after a write.
	kick chan struct{}

	mu sync.Mutex
	// client is the session's, nil while there's none: every device is then offline.
	client  *client
	devices map[string]*device
	// byURL finds a device by any of its URLs.
	byURL map[string]*device
	// running are the executions under way, and when each began.
	running map[string]time.Time
	// skipped are the widgets not served, each logged once.
	skipped map[string]bool
}

var _ plugin.Handler = (*bridge)(nil)

func newBridge(a Account, env plugin.Environment, declared []contractv1.DeclaredDevice, t timing, log *slog.Logger) *bridge {
	ids := make([]string, 0, len(declared))
	for _, d := range declared {
		ids = append(ids, d.ID)
	}
	return &bridge{
		account: a, env: env, declared: ids, timing: t, log: log, kick: make(chan struct{}, 1),
		devices: map[string]*device{}, byURL: map[string]*device{}, running: map[string]time.Time{}, skipped: map[string]bool{},
	}
}

// Serve implements plugin.Handler. It reports the devices on record as not found until the hub or
// cloud answers, then follows them, and logs in again whenever it loses them.
func (b *bridge) Serve(ctx context.Context, p *plugin.Plugin) error {
	b.plugin = p
	if err := b.publishDevices(ctx); err != nil {
		return err
	}
	// wait is the backoff after a failure, and ended after sessions that end as they begin, which a
	// login doesn't reset.
	wait, ended := b.timing.retry, b.timing.retry
	for ctx.Err() == nil {
		credential, ok, err := plugin.ReadSecret(b.env, b.account.secret())
		switch {
		case err != nil:
			b.setProblem(ctx, fmt.Sprintf("could not read the secret %q: %v", b.account.secret(), err))
			b.untilSecretChanges(ctx, "", b.timing.refused)
			continue
		case !ok || strings.TrimSpace(credential) == "":
			b.setProblem(ctx, fmt.Sprintf("waiting for the secret %s", b.account.secret()))
			b.untilSecretChanges(ctx, "", b.timing.refused)
			continue
		}
		credential = strings.TrimSpace(credential)
		var live time.Time
		err = b.session(ctx, credential, func() { wait, live = b.timing.retry, time.Now() })
		if ctx.Err() != nil {
			break
		}
		// A cloud session ends by itself, and one that lasted logs in again at once, its devices
		// online meanwhile. A credential refused then is refused for real. One that ends as soon as
		// it starts backs off, so a cloud that keeps ending them isn't asked to log in over and over.
		expired := b.account.expires() && !live.IsZero() && !errors.Is(err, errReport) && classify(err) == refused
		if expired && time.Since(live) >= b.timing.retryMax {
			b.log.Info("Overkiz ended the plugin's session, so it's logging in again", "err", err, "account", b.account.describe())
			ended = b.timing.retry
			// Its devices stay online meanwhile, and a write fails at once rather than on a dead session.
			b.mu.Lock()
			b.client = nil
			b.mu.Unlock()
			continue
		}
		b.lost(ctx)
		switch {
		case expired:
			b.log.Warn("Overkiz ended the plugin's session as it began", "err", err, "account", b.account.describe(), "retry", ended.String())
			b.setProblem(ctx, fmt.Sprintf("%s keeps ending the plugin's session: trying again in %s", b.account.describe(), ended))
			b.backoff(ctx, ended)
			ended = min(2*ended, b.timing.retryMax)
		case errors.Is(err, errReport):
			b.log.Error("Could not report Overkiz devices", "err", err, "account", b.account.describe(), "retry", wait.String())
			b.setProblem(ctx, fmt.Sprintf("could not report the devices of %s: %v", b.account.describe(), err))
			b.backoff(ctx, wait)
			wait = min(2*wait, b.timing.retryMax)
		case classify(err) == refused:
			b.log.Warn("Overkiz refused the credential", "err", err, "account", b.account.describe())
			b.setProblem(ctx, fmt.Sprintf("%s refused the login (%v): check the secret %q", b.account.describe(), err, b.account.secret()))
			b.untilSecretChanges(ctx, credential, b.timing.refused)
		case classify(err) == limited:
			b.log.Warn("Overkiz limited the plugin's requests", "err", err, "account", b.account.describe(), "retry", b.timing.limited.String())
			b.setProblem(ctx, fmt.Sprintf("%s says there were too many requests: trying again in %s", b.account.describe(), b.timing.limited))
			b.backoff(ctx, b.timing.limited)
		case certFailure(err) != nil:
			b.log.Warn("Refused Overkiz hub's certificate", "err", err, "account", b.account.describe(), "retry", wait.String())
			b.setProblem(ctx, fmt.Sprintf("%s has a certificate that %v", b.account.describe(), certFailure(err)))
			b.backoff(ctx, wait)
			wait = min(2*wait, b.timing.retryMax)
		default:
			b.log.Warn("Could not reach Overkiz", "err", err, "account", b.account.describe(), "retry", wait.String())
			b.setProblem(ctx, fmt.Sprintf("could not reach %s: %v", b.account.describe(), err))
			b.backoff(ctx, wait)
			wait = min(2*wait, b.timing.retryMax)
		}
	}
	return nil
}

// session logs in, reads the setup and follows its events until something fails. connected runs
// once the setup is read.
func (b *bridge) session(ctx context.Context, credential string, connected func()) error {
	c, err := b.account.connect(ctx, credential)
	if err != nil {
		return err
	}
	defer c.http.CloseIdleConnections()
	if err := b.readSetup(ctx, c); err != nil {
		return err
	}
	b.mu.Lock()
	b.client = c
	b.mu.Unlock()
	if err := b.publishAll(ctx); err != nil {
		return reporting(err)
	}
	b.setProblem(ctx, "")
	b.log.Info("Connected to Overkiz", "account", b.account.describe())
	connected()

	listener, err := c.register(ctx)
	if err != nil {
		return err
	}
	defer func() {
		// Best effort: a listener nobody fetches expires by itself.
		stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.timing.request)
		defer cancel()
		_ = c.unregister(stop, listener)
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.interval()):
		case <-b.kick:
		}
		events, err := c.fetch(ctx, listener)
		if err != nil && classify(err) == noListener {
			// The server forgot it, and may have dropped events with it, so read everything again.
			if listener, err = c.register(ctx); err != nil {
				return err
			}
			if err := b.readSetup(ctx, c); err != nil {
				return err
			}
			if err := b.publishAll(ctx); err != nil {
				return reporting(err)
			}
			continue
		}
		if err != nil {
			return err
		}
		reread, err := b.take(ctx, events)
		if err != nil {
			return reporting(err)
		}
		if reread {
			if err := b.readSetup(ctx, c); err != nil {
				return err
			}
			if err := b.publishAll(ctx); err != nil {
				return reporting(err)
			}
		}
	}
}

// interval is how long until the next fetch: sooner while an execution runs.
func (b *bridge) interval() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	maps.DeleteFunc(b.running, func(_ string, began time.Time) bool { return now.Sub(began) > b.timing.execution })
	if len(b.running) > 0 {
		return b.timing.busy
	}
	return b.timing.poll
}

// readSetup reads every device and keeps those it serves.
func (b *bridge) readSetup(ctx context.Context, c *client) error {
	listed, err := c.devices(ctx)
	if err != nil {
		return err
	}
	// Devices first, so a sub-device listed before its device still finds it.
	devices, byURL := map[string]*device{}, map[string]*device{}
	var subs []apiDevice
	for _, d := range listed {
		u, ok := parseURL(d.DeviceURL)
		if !ok {
			b.log.Warn("Skipped Overkiz device with an unreadable URL", "url", d.DeviceURL)
			continue
		}
		if u.sub != "" {
			subs = append(subs, d)
			continue
		}
		m, ok := models[d.widget()]
		if !ok {
			b.skip(d.widget(), d)
			continue
		}
		id, ok := u.id()
		if !ok {
			b.log.Warn("Skipped Overkiz device whose URL makes no ID", "url", d.DeviceURL)
			continue
		}
		dev := &device{
			id: id, url: d.DeviceURL, model: m, subs: map[string]string{"": d.DeviceURL},
			available: d.Available, enabled: d.Enabled, raw: states{}, values: contractv1.State{},
			info: contractv1.DiscoveredDevice{
				ID: id, Manufacturer: cmp.Or(d.attribute("core:Manufacturer"), b.account.manufacturer()), Model: d.ControllableName,
				Capabilities: slices.Clone(m.capabilities), Name: d.Label,
			},
		}
		if other, ok := devices[id]; ok {
			b.log.Warn("Skipped Overkiz device with the same ID as another", "url", d.DeviceURL, "other", other.url, "device", id)
			continue
		}
		// By the URL as listed and without a #1, which is how its sub-devices name it.
		devices[id], byURL[d.DeviceURL], byURL[u.base()] = dev, dev, dev
		dev.apply("", d.States)
	}
	for _, d := range subs {
		u, _ := parseURL(d.DeviceURL)
		dev, ok := byURL[u.base()]
		if !ok {
			// A sub-device of a device that isn't served, or a URL with #1 for the device.
			continue
		}
		dev.subs[u.sub], byURL[d.DeviceURL] = d.DeviceURL, dev
		dev.apply(u.sub, d.States)
	}
	b.mu.Lock()
	b.devices, b.byURL = devices, byURL
	b.mu.Unlock()
	return nil
}

// skip logs a widget that isn't served, once.
func (b *bridge) skip(widget string, d apiDevice) {
	b.mu.Lock()
	seen := b.skipped[widget]
	b.skipped[widget] = true
	b.mu.Unlock()
	if !seen {
		b.log.Info("Skipped Overkiz device of a kind the plugin doesn't serve", "widget", widget, "uiClass", d.UIClass,
			"controllable", d.ControllableName, "label", d.Label)
	}
}

func (d *device) online() bool { return d.available && d.enabled }

// apply takes states of the device or one of its sub-devices, and says which of its values changed.
// A value the model can no longer read keeps its last.
func (d *device) apply(sub string, reported []apiState) []string {
	for _, s := range reported {
		d.raw[stateKey{sub, s.Name}] = s.Value
	}
	var changed []string
	for c, v := range d.model.values(d.raw) {
		if old, had := d.values[c]; !had || old != v {
			d.values[c] = v
			changed = append(changed, c)
		}
	}
	slices.Sort(changed)
	return changed
}

// take applies events, and says whether the setup must be read again, as when a device is added.
func (b *bridge) take(ctx context.Context, events []apiEvent) (bool, error) {
	reread := false
	type update struct {
		id    string
		state contractv1.State
	}
	var updates []update
	availability := map[string]bool{}
	b.mu.Lock()
	for _, e := range events {
		switch e.Name {
		case eventStateChanged:
			dev, ok := b.byURL[e.DeviceURL]
			if !ok {
				continue
			}
			u, _ := parseURL(e.DeviceURL)
			if changed := dev.apply(u.sub, e.DeviceStates); len(changed) > 0 {
				s := contractv1.State{}
				for _, c := range changed {
					s[c] = dev.values[c]
				}
				updates = append(updates, update{dev.id, s})
			}
		case eventAvailable, eventUnavailable, eventDisabled:
			dev, ok := b.byURL[e.DeviceURL]
			if !ok || dev.url != e.DeviceURL {
				continue
			}
			switch e.Name {
			case eventDisabled:
				// Enabling it again has no event that pyoverkiz knows of, so the setup is read again on
				// the next added, removed or updated device, or reconnect.
				dev.enabled = false
			default:
				dev.available = e.Name == eventAvailable
			}
			availability[dev.id] = dev.online()
		case eventCreated, eventRemoved, eventUpdated:
			reread = true
		case eventExecutionChanged:
			switch e.NewState {
			case execCompleted:
				delete(b.running, e.ExecID)
			case execFailed:
				delete(b.running, e.ExecID)
				b.log.Warn("Overkiz execution failed", "exec", e.ExecID, "failure", e.FailureType)
				// What the device did instead may not have been reported, so read it again.
				reread = true
			}
		}
	}
	b.mu.Unlock()
	for _, u := range updates {
		if err := b.plugin.ReportState(ctx, u.id, u.state); err != nil {
			return false, err
		}
	}
	for _, id := range slices.Sorted(maps.Keys(availability)) {
		if err := b.plugin.SetAvailability(ctx, id, availability[id]); err != nil {
			return false, err
		}
	}
	return reread, nil
}

// lost ends the session: every device is offline, and the app can't be seen.
func (b *bridge) lost(ctx context.Context) {
	b.mu.Lock()
	b.client = nil
	clear(b.running)
	ids := slices.Sorted(maps.Keys(b.devices))
	b.mu.Unlock()
	for _, id := range ids {
		b.check(b.plugin.SetAvailability(ctx, id, false), "Could not report Overkiz device offline", id)
	}
	// Nil, not empty: an empty list would say every device was removed from the app.
	b.check(b.plugin.SetDiscovered(ctx, nil), "Could not report Overkiz devices", "")
}

func (b *bridge) check(err error, msg, id string) {
	if err != nil && !errors.Is(err, plugin.ErrClosed) && !errors.Is(err, context.Canceled) {
		b.log.Error(msg, "err", err, "device", id)
	}
}

// publishAll reports the setup: what the app holds, every device, and their availability and what
// they've reported, all from one snapshot.
func (b *bridge) publishAll(ctx context.Context) error {
	type report struct {
		id     string
		online bool
		values contractv1.State
	}
	b.mu.Lock()
	ids := slices.Sorted(maps.Keys(b.devices))
	discovered := make([]contractv1.DiscoveredDevice, 0, len(ids))
	reports := make([]report, 0, len(ids))
	for _, id := range ids {
		d := b.devices[id]
		discovered = append(discovered, d.info)
		reports = append(reports, report{id, d.online(), maps.Clone(d.values)})
	}
	devices := b.reportsLocked()
	b.mu.Unlock()
	if err := b.plugin.SetDiscovered(ctx, discovered); err != nil {
		return err
	}
	if err := b.plugin.SetDevices(ctx, devices, 0); err != nil {
		return err
	}
	for _, r := range reports {
		if err := b.plugin.SetAvailability(ctx, r.id, r.online); err != nil {
			return err
		}
		// What's known: a shutter yet to report where it is, or one the hub can't place, has no
		// position, and that's no reason to drop the rest.
		if err := b.plugin.ReportState(ctx, r.id, r.values); err != nil {
			return err
		}
	}
	return nil
}

// publishDevices reports every device served, and every device on record that isn't, as not found.
func (b *bridge) publishDevices(ctx context.Context) error {
	b.mu.Lock()
	reports := b.reportsLocked()
	b.mu.Unlock()
	return b.plugin.SetDevices(ctx, reports, 0)
}

func (b *bridge) reportsLocked() []contractv1.DeviceReport {
	reports := make([]contractv1.DeviceReport, 0, len(b.devices)+len(b.declared))
	for _, id := range slices.Sorted(maps.Keys(b.devices)) {
		d := b.devices[id]
		reports = append(reports, contractv1.DeviceReport{
			ID: id, Found: true, Manufacturer: d.info.Manufacturer, Model: d.info.Model, Capabilities: d.info.Capabilities,
		})
	}
	for _, id := range b.declared {
		if _, ok := b.devices[id]; !ok {
			reports = append(reports, contractv1.DeviceReport{ID: id})
		}
	}
	return reports
}

// setProblem sets the plugin's error, which oikade status shows.
func (b *bridge) setProblem(ctx context.Context, problem string) {
	b.check(b.plugin.SetError(ctx, problem), "Could not report the plugin's problem", "")
}

// untilSecretChanges waits until the secret is other than was, or for at most most.
func (b *bridge) untilSecretChanges(ctx context.Context, was string, most time.Duration) {
	deadline := time.Now().Add(most)
	for time.Now().Before(deadline) {
		if !sleep(ctx, b.timing.secret) {
			return
		}
		now, _, _ := plugin.ReadSecret(b.env, b.account.secret())
		if strings.TrimSpace(now) != was {
			return
		}
	}
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

// backoff waits d after a failure.
func (b *bridge) backoff(ctx context.Context, d time.Duration) {
	if b.timing.backoff != nil {
		b.timing.backoff(ctx, d)
		return
	}
	sleep(ctx, d)
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

// Set implements plugin.Handler. It returns once Overkiz has taken the command, before the device
// has moved: the device's reports then show it moving. It fails at once while there's no session.
func (b *bridge) Set(ctx context.Context, _ *plugin.Plugin, cmd contractv1.SetCommand) error {
	b.mu.Lock()
	c, dev := b.client, b.devices[cmd.ID]
	var (
		sub      string
		commands []apiCommand
		url      string
		err      error
	)
	if dev != nil {
		sub, commands, err = dev.model.write(cmd.Capability, cmd.Value, dev.raw)
		url = dev.subs[sub]
	}
	b.mu.Unlock()
	switch {
	case dev == nil:
		return fmt.Errorf("no Overkiz device %s", cmd.ID)
	case c == nil:
		return fmt.Errorf("can't reach %s", b.account.describe())
	case err != nil:
		return fmt.Errorf("could not set %s on %s: %w", cmd.Capability, cmd.ID, err)
	case len(commands) == 0:
		return nil
	case url == "":
		return fmt.Errorf("could not set %s on %s: it has no sub-device #%s", cmd.Capability, cmd.ID, sub)
	}
	ctx, cancel := context.WithTimeout(ctx, b.timing.set)
	defer cancel()
	exec, err := c.execute(ctx, url, commands...)
	if err != nil {
		return fmt.Errorf("could not set %s on %s: %w", cmd.Capability, cmd.ID, err)
	}
	b.mu.Lock()
	if exec != "" {
		b.running[exec] = time.Now()
	}
	b.mu.Unlock()
	select {
	case b.kick <- struct{}{}:
	default:
	}
	return nil
}

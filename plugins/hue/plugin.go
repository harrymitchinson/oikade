package hue

import (
	"cmp"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"oikade.dev/contract/v1"
	"oikade.dev/internal/capability"
	"oikade.dev/internal/mdns"
	"oikade.dev/sdk/plugin"
)

// Config is where the plugin finds the bridge, when mDNS can't.
type Config struct {
	// Address is the bridge's host, with its port if it isn't 443, such as 192.168.1.30, for a
	// bridge mDNS can't reach. Without it, the plugin finds the bridge over mDNS by its ID.
	Address string

	// roots is the tests' authority, in place of Philips Hue's.
	roots *x509.CertPool
}

// timing is how long the plugin waits, which tests shorten.
type timing struct {
	// retry is the first wait after the bridge doesn't answer, doubling up to retryMax.
	retry, retryMax time.Duration
	// refused is how long a key the bridge refused waits before the plugin reads it again.
	refused time.Duration
	// limited is how long to wait after the bridge says there were too many requests.
	limited time.Duration
	// find bounds looking for the bridge over mDNS, and request one request.
	find, request time.Duration
	// pace is the least time between writes, and set how long Set waits for its write's turn, which
	// must be within the host's Matter write timeout (5s).
	pace, set time.Duration
	// ping is how long the event stream may be quiet before the connection is checked, and pong how
	// long the bridge has to answer.
	ping, pong time.Duration
	// backoff waits d after a failure, or until ctx ends. Nil is sleep; a test sees the waits
	// through it.
	backoff func(ctx context.Context, d time.Duration)
}

var defaults = timing{
	retry: 5 * time.Second, retryMax: time.Minute, refused: time.Minute, limited: time.Minute,
	find: 10 * time.Second, request: 10 * time.Second, ping: 30 * time.Second, pong: 15 * time.Second,
	pace: 100 * time.Millisecond, set: 4 * time.Second,
}

// maxSkipped bounds the devices not reported, and the values left out, that are logged.
const maxSkipped = 64

// New returns the plugin's Factory, for home.Plugin.
func New(c Config) plugin.Factory {
	return func(env plugin.Environment, declared []contract.DeclaredDevice) (plugin.Handler, error) {
		log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("logger", "hue")
		return newBridge(c, env, declared, defaults, findBridge, log), nil
	}
}

// bridge is the plugin.
type bridge struct {
	config   Config
	env      plugin.Environment
	declared []string
	timing   timing
	log      *slog.Logger
	// find is where the bridge with this ID is on the network, which tests replace.
	find func(ctx context.Context, id string, within time.Duration) (string, error)

	plugin *plugin.Plugin

	mu sync.Mutex
	// home is the last read and the events since, nil while there's no session.
	home *home
	// writer is the session's, nil while there's none.
	writer *writer
	// skipped are the devices not reported that have been logged, and ignored the values left out,
	// by device and capability, each up to maxSkipped.
	skipped map[skip]bool
	ignored map[[2]string]bool
}

var _ plugin.Handler = (*bridge)(nil)

func newBridge(c Config, env plugin.Environment, declared []contract.DeclaredDevice, t timing,
	find func(context.Context, string, time.Duration) (string, error), log *slog.Logger) *bridge {
	ids := make([]string, 0, len(declared))
	for _, d := range declared {
		ids = append(ids, d.ID)
	}
	return &bridge{config: c, env: env, declared: ids, timing: t, find: find, log: log,
		skipped: map[skip]bool{}, ignored: map[[2]string]bool{}}
}

// Serve implements plugin.Handler. It reports the devices on record as not found until the bridge
// answers, then follows them, and reads the bridge again whenever it loses them.
func (b *bridge) Serve(ctx context.Context, p *plugin.Plugin) error {
	b.plugin = p
	if err := p.SetDevices(ctx, b.reports(), 0); err != nil {
		return err
	}
	wait := b.timing.retry
	for ctx.Err() == nil {
		var live time.Time
		err := b.session(ctx, func() { live = time.Now() })
		if ctx.Err() != nil {
			break
		}
		// Only a session that lasted resets the backoff, so a bridge that drops the plugin as soon as
		// it connects isn't asked again every few seconds.
		lasted := !live.IsZero() && time.Since(live) >= b.timing.retryMax
		if lasted {
			wait = b.timing.retry
		}
		// A stream that lasted and then ended is opened again at once, its devices online meanwhile.
		if errors.Is(err, errStream) && lasted {
			b.log.Info("Hue bridge's event stream ended, so the plugin is reading the bridge again", "err", err)
			continue
		}
		b.lost(ctx)
		where := b.describe()
		switch {
		case errors.Is(err, errNoKey):
			b.setProblem(ctx, "has no key for the Hue bridge yet")
			b.backoff(ctx, b.timing.retry)
		case errors.Is(err, errSecret):
			b.log.Error("Could not read the Hue bridge's key", "err", err, "retry", b.timing.refused.String())
			b.setProblem(ctx, err.Error())
			b.backoff(ctx, b.timing.refused)
		case errors.Is(err, errReport):
			b.log.Error("Could not report Hue devices", "err", err, "retry", wait.String())
			b.setProblem(ctx, fmt.Sprintf("could not report the devices: %v", err))
			b.backoff(ctx, wait)
			wait = min(2*wait, b.timing.retryMax)
		case refusedKey(err):
			b.log.Warn("Hue bridge refused the plugin's key", "err", err, "bridge", where, "retry", b.timing.refused.String())
			b.setProblem(ctx, fmt.Sprintf("%s refused the plugin's key, which may have been removed in the Hue app (%v)", where, err))
			b.backoff(ctx, b.timing.refused)
		case limitedRequests(err):
			b.log.Warn("Hue bridge limited the plugin's requests", "err", err, "bridge", where, "retry", b.timing.limited.String())
			b.setProblem(ctx, fmt.Sprintf("%s says there were too many requests: trying again in %s", where, b.timing.limited))
			b.backoff(ctx, b.timing.limited)
		case certFailure(err) != nil:
			b.log.Warn("Refused Hue bridge's certificate", "err", err, "bridge", where, "retry", wait.String())
			b.setProblem(ctx, fmt.Sprintf("%s has a certificate that %v", where, certFailure(err)))
			b.backoff(ctx, wait)
			wait = min(2*wait, b.timing.retryMax)
		case errors.Is(err, mdns.ErrNotFound):
			hint := "check it's on, or set hue.Config's Address"
			if b.config.Address != "" {
				hint = "check it's on, and hue.Config's Address"
			}
			b.log.Warn("Could not find Hue bridge", "err", err, "bridge", where, "retry", wait.String())
			b.setProblem(ctx, fmt.Sprintf("could not find %s on the network over mDNS: %s", where, hint))
			b.backoff(ctx, wait)
			wait = min(2*wait, b.timing.retryMax)
		default:
			b.log.Warn("Could not reach Hue bridge", "err", err, "bridge", where, "retry", wait.String())
			b.setProblem(ctx, fmt.Sprintf("could not reach %s: %v", where, err))
			b.backoff(ctx, wait)
			wait = min(2*wait, b.timing.retryMax)
		}
	}
	return nil
}

var (
	// errNoKey is a plugin that has no key for the bridge yet, and errSecret one whose secret can't
	// be read.
	errNoKey  = errors.New("no key for the Hue bridge")
	errSecret = errors.New("could not read the Hue bridge's key")
	// errStream is the event stream ending.
	errStream = errors.New("the event stream ended")
	// errReport is a report the SDK refused or couldn't publish: the plugin's fault, or the host's
	// connection, not the bridge's.
	errReport = errors.New("could not report to Oikade")
)

func reporting(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", errReport, err)
}

// session finds the bridge, reads it, and follows its event stream until something fails. connected
// runs once its devices are reported.
func (b *bridge) session(ctx context.Context, connected func()) error {
	k, err := b.kept()
	if err != nil {
		return err
	}
	address := b.config.Address
	if address == "" {
		if address, err = b.find(ctx, k.Bridge, b.timing.find); err != nil {
			return err
		}
	}
	c := newClient(address, k.Key, bridgeTLS(cmp.Or(b.config.roots, hueRoots), k.Bridge), b.timing)
	defer c.close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Opened before the read, so a change during it waits in the stream: applying every change since
	// the stream opened, in order, over the read leaves each value at its latest.
	stream, err := c.events(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	if err := b.read(ctx, c); err != nil {
		return err
	}
	w := newWriter(b.timing.pace, b.log)
	var writing sync.WaitGroup
	writing.Go(func() { w.run(ctx, c) })
	b.mu.Lock()
	b.writer = w
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.writer = nil
		b.mu.Unlock()
		cancel()
		writing.Wait()
		w.close()
	}()
	b.setProblem(ctx, "")
	connected()
	events := newEventReader(stream)
	for {
		data, err := events.next()
		if err != nil {
			return fmt.Errorf("%w: %w", errStream, err)
		}
		again, err := b.take(ctx, data)
		if err != nil {
			return err
		}
		if again {
			if err := b.read(ctx, c); err != nil {
				return err
			}
		}
	}
}

// kept is what setup kept: the bridge's ID and the key it gave.
type kept struct {
	Bridge string `json:"bridge"`
	Key    string `json:"key"`
}

// secret is the name of the host secret that holds what setup kept, one for each plugin a home
// imports, as each may be a different bridge.
func (b *bridge) secret() string { return b.name() + ".hue-bridge" }

func (b *bridge) kept() (kept, error) {
	raw, ok, err := plugin.ReadSecret(b.env, b.secret())
	switch {
	case err != nil:
		return kept{}, fmt.Errorf("%w: the host's secret %s: %w", errSecret, b.secret(), err)
	case !ok:
		return kept{}, errNoKey
	}
	var k kept
	if err := json.Unmarshal([]byte(raw), &k); err != nil || !bridgeID(k.Bridge) || k.Key == "" {
		return kept{}, fmt.Errorf("%w: the host's secret %s isn't a Hue bridge's ID and key", errSecret, b.secret())
	}
	k.Bridge = strings.ToUpper(k.Bridge)
	return k, nil
}

// bridgeID is whether id is a bridge's ID: 16 hex digits, such as C42996FFFEC84D62.
func bridgeID(id string) bool {
	if len(id) != 16 {
		return false
	}
	_, err := strconv.ParseUint(id, 16, 64)
	return err == nil
}

// findBridge looks for the bridge over mDNS, as the _hue._tcp instance whose bridgeid is id, and
// returns its address.
func findBridge(ctx context.Context, id string, within time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	match := func(s mdns.Service) bool { return strings.EqualFold(s.TXT["bridgeid"], id) }
	found, err := mdns.Browse(ctx, "_hue._tcp.local", func(s []mdns.Service) bool { return slices.ContainsFunc(s, match) })
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(found, match)
	if i < 0 {
		return "", fmt.Errorf("%w: nothing answered for Hue bridge %s", mdns.ErrNotFound, id)
	}
	return serviceAddress(found[i]), nil
}

// serviceAddress is where an mDNS instance is: its first IPv4 address if a response gave it, or its
// host, which a dial finds.
func serviceAddress(s mdns.Service) string {
	host := s.Host
	for _, a := range s.Addrs {
		if a.Is4() {
			host = a.String()
			break
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(int(cmp.Or(s.Port, 443))))
}

// describe is the bridge as a log or the status names it.
func (b *bridge) describe() string {
	k, err := b.kept()
	switch {
	case b.config.Address != "":
		return "the Hue bridge at " + b.config.Address
	case err == nil:
		return "the Hue bridge " + k.Bridge
	}
	return "the Hue bridge"
}

// read reads every resource, and reports what it finds.
func (b *bridge) read(ctx context.Context, c *client) error {
	raw, err := c.resources(ctx)
	if err != nil {
		return err
	}
	h, skipped, err := build(raw)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.home = h
	for _, s := range skipped {
		if b.skipped[s] || len(b.skipped) >= maxSkipped {
			continue
		}
		b.skipped[s] = true
		b.log.Info("Skipped Hue device", "model", s.model, "name", s.name, "reason", s.reason)
	}
	b.mu.Unlock()
	return reporting(b.publishAll(ctx))
}

// take applies one event from the stream, and reports whether the bridge must be read again: a
// device or room changed, or a resource the plugin reads was added or removed. One that can't be
// read is logged and changes nothing.
func (b *bridge) take(ctx context.Context, data []byte) (bool, error) {
	updates, err := parseEvents(data)
	if err != nil {
		b.log.Warn("Ignored Hue event", "err", err)
		return false, nil
	}
	again := false
	var changed []*device
	b.mu.Lock()
	h := b.home
	for _, u := range updates {
		for _, raw := range u.Data {
			var k kind
			if json.Unmarshal(raw, &k) != nil || !slices.Contains(read, k.Type) {
				continue
			}
			switch {
			case u.Type == "add", u.Type == "delete":
				again = true
				continue
			case u.Type != "update":
				continue
			case slices.Contains(structural, k.Type):
				again = true
				continue
			}
			d, err := h.apply(raw)
			if err != nil {
				b.log.Warn("Ignored Hue event", "err", err)
				continue
			}
			if d != nil && !slices.Contains(changed, d) {
				changed = append(changed, d)
			}
		}
	}
	b.mu.Unlock()
	for _, d := range changed {
		if err := b.publish(ctx, d); err != nil {
			return false, reporting(err)
		}
	}
	return again, nil
}

// publishAll reports what the bridge has: every device, and their availability and values.
func (b *bridge) publishAll(ctx context.Context) error {
	b.mu.Lock()
	ids := slices.Sorted(maps.Keys(b.home.devices))
	discovered := make([]contract.DiscoveredDevice, 0, len(ids))
	devices := make([]*device, 0, len(ids))
	for _, id := range ids {
		d := b.home.devices[id]
		discovered = append(discovered, d.info)
		devices = append(devices, d)
	}
	reports := b.reportsLocked()
	b.mu.Unlock()
	if err := b.plugin.SetDiscovered(ctx, discovered); err != nil {
		return err
	}
	if err := b.plugin.SetDevices(ctx, reports, 0); err != nil {
		return err
	}
	for _, d := range devices {
		if err := b.publish(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

// publish reports a device's availability and values, if they've changed since it last did. A light
// can always be read, so it reports every value. A sensor reports what it has.
func (b *bridge) publish(ctx context.Context, d *device) error {
	b.mu.Lock()
	h := b.home
	online, values := h.online(d), b.validLocked(d, h.values(d))
	availability := !d.published || online != d.online
	state := !d.published || !maps.Equal(values, d.values)
	d.online, d.values, d.published = online, values, true
	full := len(values) == len(d.info.Capabilities)
	b.mu.Unlock()
	if availability {
		if err := b.plugin.SetAvailability(ctx, d.id, online); err != nil {
			return err
		}
	}
	switch {
	case !state:
	case full:
		return b.plugin.SetState(ctx, d.id, values)
	case len(values) > 0:
		return b.plugin.ReportState(ctx, d.id, values)
	}
	return nil
}

// validLocked is values without any its capability doesn't take, such as a battery above 100%, which
// the SDK would refuse, so one odd value can't end the session. Each is logged once. Call with mu
// held.
func (b *bridge) validLocked(d *device, values contract.State) contract.State {
	for c, v := range values {
		if _, err := capability.Validate(c, v); err != nil {
			delete(values, c)
			if key := [2]string{d.id, c}; !b.ignored[key] && len(b.ignored) < maxSkipped {
				b.ignored[key] = true
				b.log.Warn("Ignored Hue value", "err", err, "device", d.id, "capability", c)
			}
		}
	}
	return values
}

// reports are the devices the bridge has, and those on record that it doesn't, as not found.
func (b *bridge) reports() []contract.DeviceReport {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reportsLocked()
}

func (b *bridge) reportsLocked() []contract.DeviceReport {
	var devices map[string]*device
	if b.home != nil {
		devices = b.home.devices
	}
	reports := make([]contract.DeviceReport, 0, len(devices)+len(b.declared))
	for _, id := range slices.Sorted(maps.Keys(devices)) {
		i := devices[id].info
		reports = append(reports, contract.DeviceReport{
			ID: id, Found: true, Manufacturer: i.Manufacturer, Model: i.Model, FirmwareVersion: i.FirmwareVersion,
			Capabilities: i.Capabilities, DeviceType: i.DeviceType,
		})
	}
	for _, id := range b.declared {
		if _, ok := devices[id]; !ok {
			reports = append(reports, contract.DeviceReport{ID: id})
		}
	}
	return reports
}

// lost ends the session: every device is offline, and the bridge can't be seen.
func (b *bridge) lost(ctx context.Context) {
	b.mu.Lock()
	var ids []string
	if b.home != nil {
		ids = slices.Sorted(maps.Keys(b.home.devices))
		for _, d := range b.home.devices {
			d.online = false
		}
	}
	b.mu.Unlock()
	for _, id := range ids {
		b.check(b.plugin.SetAvailability(ctx, id, false), "Could not report Hue device offline", id)
	}
	// Nil, not empty: an empty list would say every device was removed from the bridge.
	b.check(b.plugin.SetDiscovered(ctx, nil), "Could not report Hue devices", "")
}

func (b *bridge) check(err error, msg, id string) {
	if err != nil && !errors.Is(err, plugin.ErrClosed) && !errors.Is(err, context.Canceled) {
		b.log.Error(msg, "err", err, "device", id)
	}
}

// setProblem sets the plugin's error, which oikade status shows.
func (b *bridge) setProblem(ctx context.Context, problem string) {
	b.check(b.plugin.SetError(ctx, problem), "Could not report the plugin's problem", "")
}

// name is the plugin's name, as the home imports it.
func (b *bridge) name() string { return cmp.Or(b.env.Root.Plugin(), "hue") }

func (b *bridge) backoff(ctx context.Context, d time.Duration) {
	if b.timing.backoff != nil {
		b.timing.backoff(ctx, d)
		return
	}
	sleep(ctx, d)
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

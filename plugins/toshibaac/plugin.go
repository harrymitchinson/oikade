package toshibaac

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// Config is a Toshiba Home AC Control account.
type Config struct {
	// Username is the account's username in the app, usually an email address.
	Username string
	// Password is the account's password, such as oikade.Var("TOSHIBA_PASSWORD").
	Password string
	// OutdoorUnits are the indoor units that share an outdoor unit, one list for each, by the IDs
	// the plugin reports them as. Units on one outdoor unit can't heat and cool at once, so a
	// write that would run one in a mode that clashes with another's is refused. The cloud
	// doesn't say which units share one.
	OutdoorUnits [][]string

	// The tests' cloud and hub, in place of the real ones.
	api       string
	transport http.RoundTripper
	hub       hubOptions
}

// outdoorUnits are the lists in OutdoorUnits, by the device IDs the plugin reports, and the list
// each unit is in. A unit can be in only one, and a list has at least two.
func (c Config) outdoorUnits() (groups [][]string, on map[string]int, err error) {
	on = map[string]int{}
	for i, ids := range c.OutdoorUnits {
		if len(ids) < 2 {
			return nil, nil, fmt.Errorf("toshibaac.Config's OutdoorUnits has a list of %d units: each lists the units that share one outdoor unit", len(ids))
		}
		group := make([]string, 0, len(ids))
		for _, raw := range ids {
			// As a unit's DeviceUniqueId becomes its ID, so one copied from the app matches.
			id, ok := deviceID(raw)
			if !ok {
				return nil, nil, fmt.Errorf("toshibaac.Config's OutdoorUnits has %q, which isn't a unit's ID", raw)
			}
			if j, ok := on[id]; ok {
				if j == i {
					return nil, nil, fmt.Errorf("toshibaac.Config's OutdoorUnits lists %s twice on one outdoor unit", id)
				}
				return nil, nil, fmt.Errorf("toshibaac.Config's OutdoorUnits puts %s on two outdoor units", id)
			}
			on[id] = i
			group = append(group, id)
		}
		groups = append(groups, group)
	}
	return groups, on, nil
}

// timing is how often the plugin reads and how long it waits, which tests shorten.
type timing struct {
	// reload is how often every unit's state is read over HTTP, in case the hub missed a change.
	reload time.Duration
	// keepAlive is the hub connection's MQTT keep-alive.
	keepAlive time.Duration
	// retry is the first wait after the cloud doesn't answer, doubling up to retryMax.
	retry, retryMax time.Duration
	// limited is the longest wait after being told there were too many requests, and the wait when
	// the cloud doesn't say how long.
	limited time.Duration
	// refused is how long to wait before trying a refused password again, since too many failed
	// logins could lock the account.
	refused time.Duration
	// request bounds one request, and set a write, which must answer within the host's Matter
	// write timeout (5s).
	request, set time.Duration
	// renew is how long before its SAS token runs out a session starts again.
	renew time.Duration
	// intent is how long the writes sent to a unit count, until it reports: one it never
	// reports, say because it lost its WiFi, mustn't hold the units on its outdoor unit for good.
	intent time.Duration
	// backoff waits d after a failure, or until ctx ends. Nil is sleep; a test sees the waits
	// through it.
	backoff func(ctx context.Context, d time.Duration)
}

var cloudTiming = timing{
	reload: 30 * time.Minute, keepAlive: time.Minute, retry: 30 * time.Second, retryMax: 5 * time.Minute,
	limited: 10 * time.Minute, refused: time.Hour, request: 15 * time.Second,
	set: 4 * time.Second, renew: 5 * time.Minute, intent: time.Minute,
}

// capabilities are what every unit reports, sorted.
var capabilities = []string{contractv1.Mode, contractv1.On, contractv1.Setpoint, contractv1.Temperature}

// New returns the plugin's Factory, for home.Plugin.
func New(c Config) plugin.Factory {
	// Space around it, as the environment can leave, is no part of a password.
	c.Password = strings.TrimSpace(c.Password)
	return func(_ plugin.Environment, declared []contractv1.DeclaredDevice) (plugin.Handler, error) {
		switch {
		case c.Username == "":
			return nil, errors.New("toshibaac.Config has no Username")
		case c.Password == "":
			return nil, errors.New("toshibaac.Config has no Password")
		}
		log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("logger", "toshibaac")
		return newBridge(c, declared, cloudTiming, log)
	}
}

// device is one indoor unit the plugin serves.
type device struct {
	id string
	// cloudID is the unit's ID in the cloud's API, and uniqueID the one its messages carry.
	cloudID, uniqueID string
	info              contractv1.DiscoveredDevice
	// modes are the modes it does, by raw value.
	modes []byte
	state fcuState
	// intent is its state with the writes sent since its last report, which the next write is
	// worked out from, nil if there are none: a mode and then a setpoint, sent together, must
	// see the new mode.
	intent *fcuState
	// intentUntil is when intent stops counting.
	intentUntil time.Time
	// values are what was last reported.
	values contractv1.State
}

// bridge is the plugin.
type bridge struct {
	config   Config
	declared []string
	timing   timing
	log      *slog.Logger

	plugin *plugin.Plugin

	// mu guards the devices, each device's state and values, and the hub.
	mu      sync.Mutex
	devices map[string]*device
	// hub is the session's connection to the hub, nil while there's none, and hubDevice the
	// device ID the plugin is on it.
	hub       *mqttConn
	hubDevice string
	// byUnique finds a device by the ID its messages carry.
	byUnique map[string]*device
	// unknownModes are the mode bytes not served, each logged once.
	unknownModes map[byte]bool

	// outdoorUnits are the units that share each outdoor unit, and outdoor the one each is on.
	outdoorUnits [][]string
	outdoor      map[string]int
}

var _ plugin.Handler = (*bridge)(nil)

func newBridge(c Config, declared []contractv1.DeclaredDevice, t timing, log *slog.Logger) (*bridge, error) {
	groups, outdoor, err := c.outdoorUnits()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(declared))
	for _, d := range declared {
		ids = append(ids, d.ID)
	}
	return &bridge{
		config: c, declared: ids, timing: t, log: log,
		devices: map[string]*device{}, byUnique: map[string]*device{}, unknownModes: map[byte]bool{},
		outdoorUnits: groups, outdoor: outdoor,
	}, nil
}

// clientID is the mobile device the plugin registers as: one per account, the same each time, so
// the cloud doesn't collect a new one at every login.
func (c Config) clientID() string { return c.Username + "_" + c.deviceID() }

// deviceID is the Device-ID header the plugin sends with every request, as the app sends its
// Android ID: 16 hex digits, the same each time for the account.
func (c Config) deviceID() string {
	sum := sha256.Sum256([]byte("oikade:" + c.Username))
	return hex.EncodeToString(sum[:8])
}

// Serve implements plugin.Handler. It reports the devices on record as not found until the cloud
// answers, then follows them, and logs in again whenever it loses them.
func (b *bridge) Serve(ctx context.Context, p *plugin.Plugin) error {
	b.plugin = p
	if err := b.publishDevices(ctx); err != nil {
		return err
	}
	wait := b.timing.retry
	// limit is the last wait for a limit, until a session connects.
	var limit time.Duration
	for ctx.Err() == nil {
		var live time.Time
		err := b.session(ctx, func() { live = time.Now() })
		if ctx.Err() != nil {
			break
		}
		// Only a session that lasted resets the backoff, so one that fails as soon as it connects
		// isn't tried again every 30 seconds.
		if !live.IsZero() && time.Since(live) >= b.timing.retryMax {
			wait = b.timing.retry
		}
		// A session that connected ends a run of limits.
		if !live.IsZero() {
			limit = 0
		}
		// A session whose tokens ran out starts again at once, its devices online meanwhile. One that
		// ends as soon as it starts backs off, so the cloud isn't asked to log in over and over.
		if errors.Is(err, errRenew) && !live.IsZero() && time.Since(live) >= b.timing.retry {
			b.log.Info("Toshiba session ran out, so the plugin is logging in again", "err", err)
			continue
		}
		b.lost(ctx)
		switch {
		case errors.Is(err, errReport):
			b.log.Error("Could not report Toshiba units", "err", err, "retry", wait.String())
			b.setProblem(ctx, fmt.Sprintf("could not report the units: %v", err))
			b.backoff(ctx, wait)
			wait = min(2*wait, b.timing.retryMax)
		case errors.Is(err, errRefused):
			// The password is the same until a deploy restarts the plugin, which tries a new one at once.
			b.log.Warn("Toshiba's cloud refused the login", "err", err, "retry", b.timing.refused.String())
			// The fix comes first, as a status shortens a long problem.
			b.setProblem(ctx, fmt.Sprintf("Toshiba's cloud refused the login: check toshibaac.Config's Password, which is tried again in %s (%v)",
				b.timing.refused, err))
			b.backoff(ctx, b.timing.refused)
		case errors.Is(err, errLimited):
			limit = b.limitWait(err, limit)
			b.log.Warn("Toshiba's cloud limited the plugin's requests", "err", err, "retry", limit.String())
			b.setProblem(ctx, fmt.Sprintf("Toshiba's cloud says there were too many requests: trying again in %s", limit))
			b.backoff(ctx, limit)
		default:
			b.log.Warn("Could not reach Toshiba's cloud", "err", err, "retry", wait.String())
			b.setProblem(ctx, fmt.Sprintf("could not reach Toshiba's cloud: %v", err))
			b.backoff(ctx, wait)
			wait = min(2*wait, b.timing.retryMax)
		}
	}
	return nil
}

// limitWait is how long to wait after the cloud limited the plugin's requests with err, when last
// was the wait for the limit before, 0 if there wasn't one since a session connected. It's as long as
// the cloud asked (OIK-361), or twice last if that's longer, since a limit still there after the wait
// it asked for hasn't passed, and between retry and limited. One that doesn't say how long waits
// limited. last is only this process's: a deploy starts again from the cloud's ask.
func (b *bridge) limitWait(err error, last time.Duration) time.Duration {
	var status *statusError
	if !errors.As(err, &status) || !status.asked {
		return b.timing.limited
	}
	return min(max(status.wait, 2*last, b.timing.retry), b.timing.limited)
}

// session logs in, lists the units, connects to the hub and follows it until something fails or
// the tokens run out. connected runs once the units are reported.
func (b *bridge) session(ctx context.Context, connected func()) error {
	httpc := &http.Client{Transport: b.config.transport, Timeout: b.timing.request}
	defer httpc.CloseIdleConnections()
	base := cmp.Or(b.config.api, apiBase)
	a, err := login(ctx, httpc, base, b.config.deviceID(), b.config.Username, b.config.Password)
	if err != nil {
		return err
	}
	raw, err := a.register(ctx, b.config.clientID(), b.config.Username)
	if err != nil {
		return err
	}
	tok, err := parseSAS(raw)
	if err != nil {
		return err
	}
	units, err := a.units(ctx)
	if err != nil {
		return err
	}
	dialing, cancel := context.WithTimeout(ctx, b.timing.request)
	hub, err := connectHub(dialing, tok, b.config.hub, b.timing.keepAlive)
	cancel()
	if err != nil {
		return fmt.Errorf("could not connect to the hub %s: %w", tok.host, err)
	}
	defer func() { _ = hub.close() }()
	// Writes go to the hub from now, before the units are online, and fail at once when the session
	// ends.
	b.mu.Lock()
	b.hub, b.hubDevice = hub, tok.device
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.hub = nil
		b.mu.Unlock()
	}()

	// The hub is read from now, so a write's PUBACK is heard while the units are reported. Its calls
	// wait for the session's loop.
	following, stop := context.WithCancel(ctx)
	defer stop()
	ended := make(chan error, 1)
	calls := make(chan methodCall, 16)
	go func() {
		ended <- hub.run(following, func(topic string, payload []byte) {
			if call, ok := b.call(hub, topic, payload); ok {
				select {
				case calls <- call:
				case <-following.Done():
				}
			}
		})
	}()

	b.take(units)
	if err := b.publishAll(ctx); err != nil {
		return reporting(err)
	}
	b.setProblem(ctx, "")
	b.log.Info("Connected to Toshiba's cloud", "units", len(units), "hub", tok.host)
	connected()

	// A session starts again before its SAS token runs out, or halfway through a short one.
	var renew <-chan time.Time
	if !tok.expires.IsZero() {
		left := time.Until(tok.expires)
		t := time.NewTimer(max(left-min(b.timing.renew, left/2), 0))
		defer t.Stop()
		renew = t.C
	}
	reload := time.NewTicker(b.timing.reload)
	defer reload.Stop()
	// A reload runs beside the hub, so a slow cloud doesn't hold up its calls, and one at a time.
	reloads, reloaded := make(chan reading), make(chan error, 1)
	reloading := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-ended:
			return fmt.Errorf("lost the hub %s: %w", tok.host, err)
		case <-renew:
			return fmt.Errorf("%w: the hub's token runs out at %s", errRenew, tok.expires.Format(time.RFC3339))
		case call := <-calls:
			if id, changed := b.message(call); len(changed) > 0 {
				if err := b.plugin.ReportState(ctx, id, changed); err != nil {
					return reporting(err)
				}
			}
		case <-reload.C:
			if !reloading {
				reloading = true
				go func() { reloaded <- b.reloadAll(following, a, reloads) }()
			}
		case r := <-reloads:
			if changed := b.apply(r.device, "reload", func(s *fcuState) error { return s.update(r.state) }); len(changed) > 0 {
				if err := b.plugin.ReportState(ctx, r.device.id, changed); err != nil {
					return reporting(err)
				}
			}
		case err := <-reloaded:
			reloading = false
			// The hub still carries every change, so a failed read is only logged, unless the access
			// token ran out: the session then starts again.
			if errors.Is(err, errRenew) {
				return err
			}
			if err != nil && ctx.Err() == nil {
				b.log.Warn("Could not read Toshiba units' state", "err", err)
			}
		}
	}
}

// take replaces the devices with the units listed, each with the state it was listed with.
func (b *bridge) take(units []unit) {
	devices, byUnique := map[string]*device{}, map[string]*device{}
	for _, u := range units {
		id, ok := deviceID(u.UniqueID)
		if !ok {
			b.log.Warn("Skipped Toshiba unit with no usable ID", "unique", u.UniqueID, "name", u.Name)
			continue
		}
		if other, ok := devices[id]; ok {
			b.log.Warn("Skipped Toshiba unit with the same ID as another", "device", id, "name", u.Name, "other", other.info.Name)
			continue
		}
		d := &device{
			id: id, cloudID: u.ID, uniqueID: u.UniqueID, modes: unitModes(u.Merit), state: newFCUState(), values: contractv1.State{},
			info: contractv1.DiscoveredDevice{ID: id, Manufacturer: "Toshiba", Capabilities: slices.Clone(capabilities), Name: u.Name},
		}
		if u.State != "" {
			b.apply(d, "listed", func(s *fcuState) error { return s.update(u.State) })
		}
		devices[id], byUnique[u.UniqueID] = d, d
	}
	for _, id := range slices.Sorted(maps.Keys(b.outdoor)) {
		if devices[id] == nil {
			b.log.Warn("Found no Toshiba unit for an ID in OutdoorUnits", "device", id)
		}
	}
	b.mu.Lock()
	b.devices, b.byUnique = devices, byUnique
	b.mu.Unlock()
}

// deviceID is a unit's device ID, from the unique ID its messages carry: it's the unit's own, and
// stays the same when the unit is renamed. Anything a device ID can't hold becomes '-'.
func deviceID(unique string) (string, bool) {
	id := []rune(strings.ToLower(strings.TrimSpace(unique)))
	for i, r := range id {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '.' && r != '-' {
			id[i] = '-'
		}
	}
	s := strings.TrimLeft(string(id), "_.-")
	if len(s) > 63 {
		s = s[:63]
	}
	return s, contractv1.ValidID(s)
}

// apply changes a device's state, works out its values and returns those that changed. A report it
// can't read changes nothing, and is logged with what sent it. A mode the plugin doesn't know is
// logged once.
func (b *bridge) apply(d *device, from string, change func(*fcuState) error) contractv1.State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := change(&d.state); err != nil {
		b.log.Warn("Could not read Toshiba unit's report", "err", err, "device", d.id, "from", from)
		return nil
	}
	// A report of its state says what it did with the writes sent. A heartbeat only has a temperature.
	if from != cmdHeartbeat {
		d.intent = nil
	}
	values, unknown := d.state.values()
	if unknown != 0 && !b.unknownModes[unknown] {
		b.unknownModes[unknown] = true
		b.log.Info("Skipped Toshiba mode the plugin doesn't know", "mode", fmt.Sprintf("0x%02x", unknown), "device", d.id)
	}
	changed := contractv1.State{}
	for c, v := range values {
		if old, had := d.values[c]; !had || old != v {
			d.values[c], changed[c] = v, v
		}
	}
	return changed
}

// call takes one message from the hub. It answers a direct method call, and returns it if it's
// smmobile's.
func (b *bridge) call(hub *mqttConn, topic string, payload []byte) (methodCall, bool) {
	name, rid, ok := parseMethod(topic)
	if !ok {
		return methodCall{}, false
	}
	if err := answerMethod(hub, rid); err != nil {
		b.log.Warn("Could not answer Toshiba hub's method call", "err", err, "method", name)
	}
	if name != methodName {
		b.log.Debug("Ignored Toshiba hub's call to another method", "method", name)
		return methodCall{}, false
	}
	var call methodCall
	if err := json.Unmarshal(payload, &call); err != nil {
		b.log.Warn("Could not read Toshiba hub's method call", "err", err)
		return methodCall{}, false
	}
	return call, true
}

// message applies a call from the hub to the unit it's from, and returns what changed.
func (b *bridge) message(call methodCall) (string, contractv1.State) {
	b.mu.Lock()
	d := b.byUnique[call.SourceID]
	b.mu.Unlock()
	if d == nil {
		return "", nil
	}
	switch call.Command {
	case cmdState:
		return d.id, b.apply(d, call.Command, func(s *fcuState) error {
			var p struct {
				Data string `json:"data"`
			}
			if err := json.Unmarshal(call.Payload, &p); err != nil {
				return err
			}
			return s.update(p.Data)
		})
	case cmdHeartbeat:
		return d.id, b.apply(d, call.Command, func(s *fcuState) error {
			// Only iTemp: the rest of a heartbeat can be of any type.
			var p struct {
				ITemp *string `json:"iTemp"`
			}
			if err := json.Unmarshal(call.Payload, &p); err != nil {
				return err
			}
			if p.ITemp != nil {
				return s.heartbeat(*p.ITemp)
			}
			return nil
		})
	}
	return "", nil
}

// reading is a unit's state, read again.
type reading struct {
	device *device
	state  string
}

// reloadAll reads every unit's state again, and passes each on to readings. It stops at the first
// that fails.
func (b *bridge) reloadAll(ctx context.Context, a *api, readings chan<- reading) error {
	b.mu.Lock()
	devices := slices.Collect(maps.Values(b.devices))
	b.mu.Unlock()
	slices.SortFunc(devices, func(x, y *device) int { return strings.Compare(x.id, y.id) })
	for _, d := range devices {
		state, err := a.state(ctx, d.cloudID)
		if err != nil {
			return err
		}
		select {
		case readings <- reading{d, state}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// lost ends the session: every device is offline, and the account can't be seen.
func (b *bridge) lost(ctx context.Context) {
	b.mu.Lock()
	ids := slices.Sorted(maps.Keys(b.devices))
	b.mu.Unlock()
	for _, id := range ids {
		b.check(b.plugin.SetAvailability(ctx, id, false), "Could not report Toshiba unit offline", id)
	}
	// Nil, not empty: an empty list would say every unit was removed from the account.
	b.check(b.plugin.SetDiscovered(ctx, nil), "Could not report Toshiba units", "")
}

func (b *bridge) check(err error, msg, id string) {
	if err != nil && !errors.Is(err, plugin.ErrClosed) && !errors.Is(err, context.Canceled) {
		b.log.Error(msg, "err", err, "device", id)
	}
}

// publishAll reports what the account holds, every device, and their availability and values.
func (b *bridge) publishAll(ctx context.Context) error {
	type report struct {
		id     string
		values contractv1.State
	}
	b.mu.Lock()
	ids := slices.Sorted(maps.Keys(b.devices))
	discovered := make([]contractv1.DiscoveredDevice, 0, len(ids))
	reports := make([]report, 0, len(ids))
	for _, id := range ids {
		d := b.devices[id]
		discovered = append(discovered, d.info)
		reports = append(reports, report{id, maps.Clone(d.values)})
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
		if err := b.plugin.SetAvailability(ctx, r.id, true); err != nil {
			return err
		}
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
			ID: id, Found: true, Manufacturer: d.info.Manufacturer, Capabilities: d.info.Capabilities,
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

// errReport is a report the SDK refused or couldn't publish: the plugin's fault, or the host's
// connection, not the cloud's.
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

// sleep waits for d, or until ctx ends.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Set implements plugin.Handler. It sends the unit the change, and returns once the hub has taken
// it, before the unit has changed: the unit's report then shows it. It fails at once while there's
// no session.
func (b *bridge) Set(ctx context.Context, _ *plugin.Plugin, cmd contractv1.SetCommand) error {
	b.mu.Lock()
	d, hub, device := b.devices[cmd.ID], b.hub, b.hubDevice
	var (
		c   command
		err error
	)
	if d != nil {
		c, err = d.current().command(cmd.Capability, cmd.Value, d.modes)
		if err == nil {
			err = b.clashLocked(d, c)
		}
		if err == nil && hub != nil {
			// The write counts from before it's sent, so a write to another unit on its outdoor
			// unit, checked meanwhile, sees it. It counts even if the hub's PUBACK doesn't come,
			// since the hub may have taken it, until the unit reports or intent runs out.
			intent := d.current()
			// A command is a state with only what changes, so it applies as a report would.
			_ = intent.update(c.encode())
			d.intent, d.intentUntil = &intent, time.Now().Add(b.timing.intent)
		}
	}
	b.mu.Unlock()
	switch {
	case d == nil:
		return fmt.Errorf("no Toshiba unit %s", cmd.ID)
	case hub == nil:
		return errors.New("can't reach Toshiba's cloud")
	case err != nil:
		return fmt.Errorf("could not set %s on %s: %w", cmd.Capability, cmd.ID, err)
	}
	ctx, cancel := context.WithTimeout(ctx, b.timing.set)
	defer cancel()
	if err := hub.publishAcked(ctx, telemetryTopic(device), commandMessage(device, d.uniqueID, c.encode())); err != nil {
		return fmt.Errorf("could not set %s on %s: %w", cmd.Capability, cmd.ID, err)
	}
	return nil
}

// clashLocked refuses a command that would run d in a mode that clashes with another unit running on
// its outdoor unit, as each will be once the writes sent to it are done. Only one that turns d on
// or changes its mode is checked: turning it off or changing its setpoint never makes a clash.
func (b *bridge) clashLocked(d *device, c command) error {
	i, shares := b.outdoor[d.id]
	if !shares || (c.status != statusOn && c.mode == noneByte) {
		return nil
	}
	after := d.current()
	_ = after.update(c.encode())
	if after.status != statusOn {
		return nil
	}
	for _, id := range b.outdoorUnits[i] {
		o := b.devices[id]
		if o == nil || o == d {
			continue
		}
		other := o.current()
		if _, known := modes[other.mode]; other.status != statusOn || !known {
			continue
		}
		// It would run in whatever mode it last had, which could be either.
		if after.mode == noneByte {
			return fmt.Errorf("its mode isn't known yet, and it shares its outdoor unit with %s, which is running %s", id, modes[other.mode])
		}
		if clashes(after.mode, other.mode) {
			return fmt.Errorf("it shares its outdoor unit with %s, which is running %s, and %s and %s can't run at once",
				id, modes[other.mode], modes[after.mode], modes[other.mode])
		}
	}
	return nil
}

// current is the unit's state with the writes sent since its last report, while they count. b.mu
// must be held.
func (d *device) current() fcuState {
	if d.intent != nil && time.Now().Before(d.intentUntil) {
		return *d.intent
	}
	return d.state
}

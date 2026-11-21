package ewelinklan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"oikade.dev/contract/v1"
	"oikade.dev/internal/mdns"
	"oikade.dev/sdk/plugin"
)

// maxResponse is the most a device's answer to a command may be.
const maxResponse = 1 << 16

// maxSkipped bounds the kinds of device not reported that are logged.
const maxSkipped = 64

// timing is how long things take, which the tests shorten.
type timing struct {
	// settle is how long the first report waits, listening. Every device that answers is then in it,
	// including any you removed, which the host doesn't bring back if it's there from the start.
	settle time.Duration
	// requery is when the question is asked again while settling, in case a packet was lost.
	requery time.Duration
	// sweep is how often devices are looked at, for those gone quiet and keys that have arrived.
	sweep time.Duration
	// query is how often devices are asked for. It's well inside relayAge, since nothing shows that an
	// idle device speaks unless it's asked.
	query time.Duration
	// relayAge is how old a relay report can be while the device still counts as online.
	relayAge time.Duration
	// http bounds posting a command, and confirm waiting for the report that shows it worked.
	http, confirm time.Duration
}

var defaults = timing{
	settle: 5 * time.Second, requery: time.Second, sweep: 5 * time.Second, query: 15 * time.Second,
	relayAge: 30 * time.Second, http: 5 * time.Second, confirm: 10 * time.Second,
}

// Config is the developer app setup signs in with, and which devices have a humidity sensor.
type Config struct {
	// Humidity lists the devices that also announce humidity, by ID. A device's sensors decide what
	// the Home app shows, so they're stated rather than worked out from its reports.
	Humidity []string
	// AppID is your eWeLink developer app's ID, such as oikade.Var("EWELINK_APP_ID"). Only setup
	// needs it and AppSecret, to sign in to your eWeLink account and fetch each device's LAN key.
	// The plugin works on the LAN without them once the host has the keys.
	AppID string
	// AppSecret is that app's secret, such as oikade.Var("EWELINK_APP_SECRET").
	AppSecret string
}

// lan is the network the plugin listens on: mdns.Listener, or a test's.
type lan interface {
	Read(b []byte) (int, error)
	Send(b []byte) error
	Close() error
}

// New returns the plugin's Factory, for home.Plugin.
func New(c Config) plugin.Factory {
	return func(env plugin.Environment, declared []contract.DeclaredDevice) (plugin.Handler, error) {
		return newBridge(c, env, declared, func() (lan, error) { return mdns.ListenMulticast() }, defaults, time.Now,
			slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("logger", "ewelink-lan")), nil
	}
}

// device is one device that's been heard.
type device struct {
	id   string
	kind kind
	// humidity is whether it's listed in Config.Humidity and can have that sensor.
	humidity bool
	service  *service

	// secret is its LAN key as last read, and key what that derives, nil until there is one.
	secret string
	key    []byte
	// problem is what's wrong, as oikade status shows it.
	problem string
	// firmware is its firmware version, as its last report that said it, empty until one does.
	firmware string
	// lastCipher is the last report decoded, since a device repeats itself and that isn't news.
	lastCipher string
	// relayAt is when a report last said what the relay is, zero if the last one couldn't be believed.
	relayAt time.Time
	on      bool
	// reports counts those reports, and changed is closed by the next one.
	reports uint64
	changed chan struct{}
	// sequence numbers commands, always rising.
	sequence int64
	// online is what was last published, and published whether anything has been.
	online, published bool
	// warned is whether a report without humidity has been logged.
	warned bool
}

// bridge is the plugin.
type bridge struct {
	humidity map[string]bool
	env      plugin.Environment
	declared []string
	// named are the declared devices the host has under a name other than their ID.
	named  map[string]bool
	listen func() (lan, error)
	timing timing
	now    func() time.Time
	log    *slog.Logger
	client *http.Client
	cloud  cloud
	// app is your eWeLink developer app, from Config, which only setup uses.
	app app

	// ctx and plugin are set before anything can be heard.
	ctx    context.Context
	plugin *plugin.Plugin
	lan    lan

	mu sync.Mutex
	// devices are those heard, by ID, kept until the plugin stops: a device that goes quiet is offline,
	// not gone.
	devices map[string]*device
	records *records
	// skipped are the types not reported, each logged once, up to maxSkipped. full is whether a
	// device was ignored for there being maxServices already.
	skipped map[string]bool
	full    bool
	// settled is whether the first report has gone out, and dirty whether another is due.
	settled, dirty bool
	// early is what devices said before the first report, which goes out with it.
	early map[string]contract.State
}

var _ plugin.Handler = (*bridge)(nil)

func newBridge(c Config, env plugin.Environment, declared []contract.DeclaredDevice, listen func() (lan, error),
	t timing, now func() time.Time, log *slog.Logger,
) *bridge {
	humidity := map[string]bool{}
	for _, id := range c.Humidity {
		humidity[strings.ToLower(id)] = true
	}
	ids := make([]string, 0, len(declared))
	named := map[string]bool{}
	for _, d := range declared {
		ids = append(ids, d.ID)
		named[d.ID] = d.Name != d.ID
	}
	return &bridge{
		humidity: humidity, env: env, declared: ids, named: named, listen: listen, timing: t, now: now, log: log,
		client: &http.Client{Timeout: t.http}, cloud: ewelink,
		app:     app{id: strings.TrimSpace(c.AppID), secret: strings.TrimSpace(c.AppSecret)},
		devices: map[string]*device{}, records: newRecords(), skipped: map[string]bool{}, early: map[string]contract.State{},
	}
}

// Serve implements plugin.Handler.
func (b *bridge) Serve(ctx context.Context, p *plugin.Plugin) error {
	b.ctx, b.plugin = ctx, p
	conn, err := b.listen()
	if err != nil {
		return fmt.Errorf("could not listen for devices: %w", err)
	}
	b.lan = conn
	packets := make(chan []byte, 64)
	failed := make(chan error, 1)
	go func() {
		buf := make([]byte, 9000)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				failed <- err
				return
			}
			select {
			case packets <- bytes.Clone(buf[:n]):
			case <-ctx.Done():
				failed <- ctx.Err()
				return
			}
		}
	}()
	// Closing is what ends the read.
	defer func() { _ = conn.Close() }()

	b.query()
	settle := time.After(b.timing.settle)
	requery := time.After(b.timing.requery)
	sweep := time.NewTicker(b.timing.sweep)
	defer sweep.Stop()
	asked := b.now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failed:
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("lost the network: %w", err)
		case packet := <-packets:
			b.take(packet)
		case <-requery:
			b.query()
		case <-settle:
			b.mu.Lock()
			b.settled, b.dirty = true, true
			b.mu.Unlock()
			b.publish(nil)
		case <-sweep.C:
			if b.sweep() || b.now().Sub(asked) >= b.timing.query {
				asked = b.now()
				b.query()
			}
		}
	}
}

func (b *bridge) query() {
	b.mu.Lock()
	packet, err := b.records.query()
	b.mu.Unlock()
	if err == nil {
		err = b.lan.Send(packet)
	}
	b.check(err, "Could not ask for devices", "")
}

func (b *bridge) check(err error, msg, id string) {
	if err == nil || errors.Is(err, plugin.ErrClosed) || b.ctx.Err() != nil {
		return
	}
	if id != "" {
		b.log.Error(msg, "device", id, "err", err)
		return
	}
	b.log.Error(msg, "err", err)
}

// observed is what a report said that needs publishing.
type observed struct {
	id     string
	values contract.State
}

func (b *bridge) take(packet []byte) {
	now := b.now()
	b.mu.Lock()
	touched, added := b.records.take(packet, now)
	var heard []observed
	slices.Sort(touched)
	for _, name := range touched {
		if r, ok := b.accept(b.records.services[name], now); ok {
			heard = append(heard, r)
		}
	}
	b.mu.Unlock()
	for _, name := range added {
		q, err := pack(ofService(name))
		if err == nil {
			err = b.lan.Send(q)
		}
		b.check(err, "Could not ask for device", name)
	}
	b.publish(heard)
}

// accept takes what a service says now. Call with mu held. ok says a report was decoded, and is what
// to publish.
func (b *bridge) accept(s *service, now time.Time) (observed, bool) {
	if s.txt == nil {
		return observed{}, false
	}
	if id := strings.ToLower(s.txt["id"]); id != s.id {
		return observed{}, false
	}
	k, known := kinds[s.txt["type"]]
	if !known || s.txt["encrypt"] != "true" {
		// Each kind is logged once, and only so many, since the network says what they are.
		if why := s.txt["type"] + "/" + s.txt["encrypt"]; !b.skipped[why] && len(b.skipped) < maxSkipped {
			b.skipped[why] = true
			b.log.Info("Ignored eWeLink LAN device", "device", s.id, "type", s.txt["type"], "encrypt", s.txt["encrypt"])
		}
		return observed{}, false
	}
	d := b.devices[s.id]
	if d == nil {
		if len(b.devices) >= maxServices {
			if !b.full {
				b.full = true
				b.log.Warn("Ignored eWeLink LAN devices beyond the limit", "device", s.id, "limit", maxServices)
			}
			return observed{}, false
		}
		d = &device{id: s.id, kind: k, humidity: k.sensors && b.humidity[s.id], changed: make(chan struct{})}
		b.devices[s.id] = d
		b.dirty = true
		b.loadKey(d)
	}
	d.service = s
	return b.decode(d, s, now)
}

// loadKey reads a device's key again, and says whether it changed. Call with mu held. The value is
// never logged.
func (b *bridge) loadKey(d *device) bool {
	secret, ok, err := plugin.ReadSecret(b.env, d.id)
	if err != nil {
		b.setProblem(d, fmt.Sprintf("could not read its LAN key: %v", err))
		return false
	}
	secret = strings.TrimRight(secret, "\r\n")
	if !ok || secret == "" {
		d.secret, d.key = "", nil
		b.setProblem(d, "no LAN key: oikade setup "+b.name())
		return false
	}
	if secret == d.secret {
		return false
	}
	// What was decoded under the last key proves nothing about this one.
	d.secret, d.key, d.lastCipher = secret, lanKey(secret), ""
	return true
}

// setProblem records what's wrong with a device. Call with mu held.
func (b *bridge) setProblem(d *device, problem string) {
	if d.problem != problem {
		d.problem, b.dirty = problem, true
	}
}

// decode reads a device's latest report. Call with mu held.
func (b *bridge) decode(d *device, s *service, heard time.Time) (observed, bool) {
	signature := s.txt["iv"] + ":" + s.txt["data1"] + s.txt["data2"] + s.txt["data3"] + s.txt["data4"]
	if signature == d.lastCipher {
		return b.repeated(d, heard)
	}
	if d.key == nil && !b.loadKey(d) {
		return observed{}, false
	}
	iv, data, err := ciphertext(s.txt)
	if err != nil {
		// A damaged report says nothing about the key.
		return observed{}, false
	}
	params, err := decrypt(iv, data, d.key)
	if err != nil && b.loadKey(d) {
		params, err = decrypt(iv, data, d.key)
	}
	if d.key == nil {
		return observed{}, false
	}
	if err != nil {
		// While good reports are arriving, one bad one is damage. A key that stopped working shows
		// once they've stopped.
		if !b.relayFresh(d, heard) {
			b.setProblem(d, "wrong LAN key: oikade setup "+b.name())
		}
		return observed{}, false
	}
	b.setProblem(d, "")
	d.lastCipher = signature
	return b.apply(d, params, heard)
}

// repeated takes a report heard again. It's news of the device, which is still there and still says
// this, and of nothing else: it confirms no command. Call with mu held.
func (b *bridge) repeated(d *device, heard time.Time) (observed, bool) {
	b.setProblem(d, "")
	if d.relayAt.IsZero() {
		return observed{}, false
	}
	if heard.After(d.relayAt) {
		d.relayAt = heard
	}
	return observed{id: d.id, values: contract.State{}}, true
}

// apply takes a decoded report. Call with mu held.
func (b *bridge) apply(d *device, params map[string]any, heard time.Time) (observed, bool) {
	if fw, ok := params[fieldFirmware].(string); ok && fw != "" && len(fw) <= maxFirmware && fw != d.firmware {
		d.firmware, b.dirty = fw, true
	}
	r := d.kind.read(params)
	values := contract.State{}
	switch {
	case r.relay && r.bad:
		d.relayAt = time.Time{}
	case r.relay:
		d.relayAt, d.on = heard, r.on
		d.reports++
		close(d.changed)
		d.changed = make(chan struct{})
		values[contract.On] = r.on
	}
	announced := capabilities(d)
	for capability, value := range r.values {
		if slices.Contains(announced, capability) {
			values[capability] = value
		}
	}
	// A probe that can't measure humidity, such as a temperature-only one, says "unavailable".
	if _, ok := r.values[contract.Humidity]; d.humidity && !ok && !d.warned {
		d.warned = true
		b.log.Info("Device listed for humidity sent no humidity reading", "device", d.id,
			"humidity", fmt.Sprint(params[fieldHumidity]), "sensor", fmt.Sprint(params["sensorType"]))
	}
	if len(values) == 0 {
		return observed{}, false
	}
	return observed{id: d.id, values: values}, true
}

func (b *bridge) relayFresh(d *device, now time.Time) bool {
	return !d.relayAt.IsZero() && !now.Before(d.relayAt) && now.Sub(d.relayAt) <= b.timing.relayAge
}

func capabilities(d *device) []string {
	if d.humidity {
		return append(slices.Clone(d.kind.capabilities), contract.Humidity)
	}
	return d.kind.capabilities
}

// sweep notices devices that have gone quiet and keys that have arrived, and says whether a key did,
// so that its device can be asked at once.
func (b *bridge) sweep() bool {
	b.mu.Lock()
	var heard []observed
	arrived := false
	for _, id := range slices.Sorted(maps.Keys(b.devices)) {
		d := b.devices[id]
		// The report that got no further is still there, so a new key needs no new report.
		if d.problem != "" {
			arrived = b.loadKey(d) || arrived
			if d.key != nil && d.service != nil {
				if r, ok := b.decode(d, d.service, d.service.heard); ok {
					heard = append(heard, r)
				}
			}
		}
	}
	b.mu.Unlock()
	b.publish(heard)
	return arrived
}

// publish sends what's changed: the device list if it has, then each availability that has, then
// these readings. Nothing goes out before the first report. It runs only on Serve's goroutine, so
// what's published stays in order.
func (b *bridge) publish(heard []observed) {
	now := b.now()
	b.mu.Lock()
	if !b.settled {
		for _, o := range heard {
			if b.early[o.id] == nil {
				b.early[o.id] = contract.State{}
			}
			maps.Copy(b.early[o.id], o.values)
		}
		b.mu.Unlock()
		return
	}
	for _, id := range slices.Sorted(maps.Keys(b.early)) {
		heard = append([]observed{{id: id, values: b.early[id]}}, heard...)
	}
	clear(b.early)
	var reports []contract.DeviceReport
	needs := false
	if b.dirty {
		b.dirty = false
		reports, needs = b.reportsLocked(), b.needsSetupLocked()
	}
	type change struct {
		id     string
		online bool
	}
	var changes []change
	for _, id := range slices.Sorted(maps.Keys(b.devices)) {
		d := b.devices[id]
		online := d.problem == "" && b.relayFresh(d, now)
		if !d.published || online != d.online {
			d.online, d.published = online, true
			changes = append(changes, change{id, online})
		}
	}
	b.mu.Unlock()

	if reports != nil {
		// The SDK sends needsSetup with the devices: with the first list, and after it at once.
		b.check(b.plugin.SetNeedsSetup(b.ctx, needs), "Could not report whether setup is needed", "")
		b.check(b.plugin.SetDevices(b.ctx, reports, 0), "Could not report devices", "")
		b.offlineUnheard(reports)
	}
	for _, c := range changes {
		b.check(b.plugin.SetAvailability(b.ctx, c.id, c.online), "Could not report availability", c.id)
	}
	for _, r := range heard {
		b.check(b.plugin.ReportState(b.ctx, r.id, r.values), "Could not report state", r.id)
	}
}

// offlineUnheard says the devices on record that haven't been heard are offline.
func (b *bridge) offlineUnheard(reports []contract.DeviceReport) {
	for _, r := range reports {
		if !r.Found {
			b.check(b.plugin.SetAvailability(b.ctx, r.ID, false), "Could not report availability", r.ID)
		}
	}
}

// reportsLocked lists everything heard, and everything on record that hasn't been. Call with mu held.
func (b *bridge) reportsLocked() []contract.DeviceReport {
	reports := make([]contract.DeviceReport, 0, len(b.devices)+len(b.declared))
	for _, id := range slices.Sorted(maps.Keys(b.devices)) {
		d := b.devices[id]
		reports = append(reports, contract.DeviceReport{
			ID: id, Found: true, FirmwareVersion: d.firmware, Capabilities: capabilities(d), Error: d.problem,
		})
	}
	for _, id := range b.declared {
		if b.devices[id] == nil {
			reports = append(reports, contract.DeviceReport{ID: id})
		}
	}
	return reports
}

// Set implements plugin.Handler. It returns once a fresh report shows the relay where it was asked
// to go, and that report is what's published.
func (b *bridge) Set(ctx context.Context, _ *plugin.Plugin, cmd contract.SetCommand) error {
	on, ok := cmd.Value.(bool)
	if cmd.Capability != contract.On || !ok {
		return fmt.Errorf("device %s can't set %s", cmd.ID, cmd.Capability)
	}
	b.mu.Lock()
	d := b.devices[cmd.ID]
	if d == nil || d.service == nil {
		b.mu.Unlock()
		return fmt.Errorf("device %s hasn't been heard on the network", cmd.ID)
	}
	if d.key == nil {
		b.mu.Unlock()
		return fmt.Errorf("device %s has no LAN key", cmd.ID)
	}
	host, port, known := b.records.address(d.service)
	if !known {
		b.mu.Unlock()
		return fmt.Errorf("device %s has no known address", cmd.ID)
	}
	d.sequence = max(d.sequence+1, b.now().UnixMilli())
	key, sequence, before, k := d.key, d.sequence, d.reports, d.kind
	// A device told what it already is may have nothing new to say.
	already := b.relayFresh(d, b.now()) && d.on == on
	b.mu.Unlock()

	if err := b.post(ctx, cmd.ID, net.JoinHostPort(host, strconv.Itoa(int(port))), k, on, key, sequence); err != nil {
		return err
	}
	if already {
		return nil
	}
	return b.confirm(ctx, d, before, on)
}

// post sends a command to a device, and returns once it's accepted it.
func (b *bridge) post(ctx context.Context, id, address string, k kind, on bool, key []byte, sequence int64) error {
	iv, data, err := encrypt(k.command(on), key)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"deviceid": id, "encrypt": true, "selfApikey": "123", "sequence": strconv.FormatInt(sequence, 10),
		"iv": iv, "data": data,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+address+k.path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Close = true
	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("device %s: %w", id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("device %s answered %s", id, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return fmt.Errorf("device %s: %w", id, err)
	}
	if len(raw) > maxResponse {
		return fmt.Errorf("device %s sent too long an answer", id)
	}
	var answer struct {
		Error *int `json:"error"`
	}
	if json.Unmarshal(raw, &answer) != nil || answer.Error == nil || *answer.Error != 0 {
		return fmt.Errorf("device %s rejected the command", id)
	}
	return nil
}

// confirm waits for a report newer than before that has the relay at on.
func (b *bridge) confirm(ctx context.Context, d *device, before uint64, on bool) error {
	limit := time.NewTimer(b.timing.confirm)
	defer limit.Stop()
	for {
		b.mu.Lock()
		done := d.reports > before && d.on == on
		changed := d.changed
		b.mu.Unlock()
		if done {
			return nil
		}
		select {
		case <-changed:
		case <-limit.C:
			return fmt.Errorf("device %s didn't report the change within %s", d.id, b.timing.confirm)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

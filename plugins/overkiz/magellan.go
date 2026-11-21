package overkiz

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"oikade.dev/contract/v1"
)

// Magellan is Atlantic's own cloud beside Overkiz, behind the same Cozytouch login, where an
// account's newer WiFi devices are. It has no events, so the plugin reads the whole setup every
// read interval, and soon after a write. Its API isn't published: this follows what Atlantic's app
// does, as the Homey app NicolasYDDER/homey-cozytouch has it, and a real account's answers.
//
// A device's capabilities are numbered, with string values, and what a number means depends on
// the product: magellanModels says, by model.

// magellanClient speaks Magellan's API with one Atlantic token: Overkiz's client, with Magellan's
// errors.
type magellanClient struct{ *client }

func newMagellanClient(httpc *http.Client, base, token string) *magellanClient {
	return &magellanClient{&client{
		http: httpc, base: base, failed: magellanError,
		authorize: func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) },
	}}
}

// mSetup is one setup in /magellan/cozytouch/setupviewv2: its devices, the zones they're in and the
// gateways they reach it through.
type mSetup struct {
	Devices  []mDevice  `json:"devices"`
	Zones    []mZone    `json:"zones"`
	Gateways []mGateway `json:"gateways"`
}

type mDevice struct {
	DeviceID     flexID        `json:"deviceId"`
	Name         string        `json:"name"`
	ModelID      flexID        `json:"modelId"`
	ProductID    flexID        `json:"productId"`
	ModelFamily  string        `json:"modelFamily"`
	ZoneID       flexID        `json:"zoneId"`
	GatewayID    flexID        `json:"gatewayId"`
	IsAvailable  *bool         `json:"isAvailable"`
	Capabilities []mCapability `json:"capabilities"`
}

type mCapability struct {
	ID    flexID     `json:"capabilityId"`
	Value flexString `json:"value"`
}

type mZone struct {
	ID   flexID `json:"id"`
	Name string `json:"name"`
}

type mGateway struct {
	ID      flexID `json:"id"`
	IsAlive *bool  `json:"isAlive"`
}

// flexID is an ID Magellan gives as a number in one place and a string in another.
type flexID string

func (f *flexID) UnmarshalJSON(raw []byte) error {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		*f = flexID(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return err
	}
	*f = flexID(n.String())
	return nil
}

// flexString is a capability's value: a string, though anything else is kept as its JSON.
type flexString string

func (f *flexString) UnmarshalJSON(raw []byte) error {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		*f = flexString(s)
		return nil
	}
	*f = flexString(strings.TrimSpace(string(raw)))
	return nil
}

// magellanError is an answer that failed, with Magellan's message, such as
// {"code":"900901","message":"Invalid Credentials"} for a token it doesn't take.
func magellanError(status int, raw []byte) error {
	var body struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	if json.Unmarshal(raw, &body) != nil || body.Message == "" {
		// Not Magellan's own answer, such as a proxy's error page.
		return &apiError{Status: status, Code: codeNotMagellan}
	}
	return &apiError{Status: status, Code: body.Type, Message: body.Message}
}

// codeNotMagellan marks a failed answer that isn't in Magellan's form.
const codeNotMagellan = "not Magellan's answer"

// mForbidden is Magellan's type for a resource the token may not reach.
const mForbidden = "ResourceAccessForbidden"

// setup reads every setup on the account. Magellan answers with a list of them, or one.
func (c *magellanClient) setup(ctx context.Context) ([]mSetup, error) {
	var raw json.RawMessage
	err := c.call(ctx, http.MethodGet, "/magellan/cozytouch/setupviewv2", nil, &raw)
	var e *apiError
	switch {
	case errors.As(err, &e) && e.Status == http.StatusNotFound && e.Code != codeNotMagellan:
		// Magellan's own answer that it has no setup: the account's devices are on Overkiz.
		return nil, fmt.Errorf("%w: %w", errNoAccount, err)
	case errors.As(err, &e) && e.Status == http.StatusForbidden && e.Code != mForbidden:
		// A read the cloud won't let this token make now: wait as for a limit.
		e.Code = codeLimited
		return nil, err
	case err != nil:
		return nil, err
	}
	raw = bytes.TrimSpace(raw)
	if bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	if len(raw) > 0 && raw[0] == '{' {
		var one mSetup
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, fmt.Errorf("reading the setup: %w", err)
		}
		return []mSetup{one}, nil
	}
	var setups []mSetup
	if err := json.Unmarshal(raw, &setups); err != nil {
		return nil, fmt.Errorf("reading the setup: %w", err)
	}
	return setups, nil
}

// write sets one capability, and returns the execution's ID once the cloud has taken it.
func (c *magellanClient) write(ctx context.Context, device string, capability int, value string) (string, error) {
	var id any = device
	if n, err := strconv.ParseInt(device, 10, 64); err == nil {
		id = n
	}
	var raw json.RawMessage
	if err := c.call(ctx, http.MethodPost, "/magellan/executions/writecapability",
		map[string]any{"capabilityId": capability, "deviceId": id, "value": value}, &raw); err != nil {
		return "", err
	}
	// The execution's ID, bare, though an object with executionId is read too.
	var answer struct {
		ExecutionID flexID `json:"executionId"`
	}
	if json.Unmarshal(raw, &answer) == nil && answer.ExecutionID != "" {
		return string(answer.ExecutionID), nil
	}
	var exec flexID
	if json.Unmarshal(raw, &exec) != nil {
		return "", nil
	}
	return string(exec), nil
}

// An execution's states: under way, done, and refused, as a write that came while the device's last
// was under way did (OIK-339). Any other ends it too, unheard of so far.
const (
	execPending    = 1
	execInProgress = 2
	execDone       = 3
	execRefused    = 4
)

// execution reads how far an execution has got.
func (c *magellanClient) execution(ctx context.Context, id string) (int, error) {
	var out struct {
		State int `json:"state"`
	}
	err := c.call(ctx, http.MethodGet, "/magellan/executions/"+id, nil, &out)
	return out.State, err
}

// capabilities are a device's capabilities, by number, as Magellan gives them.
type capabilities map[int]string

func (c capabilities) number(id int) (float64, bool) {
	v, ok := c[id]
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	return f, err == nil
}

type mWrite struct {
	capability int
	value      string
}

// mQueued is a write waiting its turn: Magellan fails a write to a device while its last runs, so
// each waits for the one before to end.
type mQueued struct {
	// id is the plugin's ID for the device, deviceID Magellan's, and set the capability the command
	// that asked for it set.
	id, deviceID, set string
	mWrite
}

// mExecution is a write Magellan has taken, under way.
type mExecution struct {
	mQueued
	began time.Time
}

// mModel is what Oikade serves of one kind of Magellan device.
type mModel struct {
	capabilities []string
	// values reads the capabilities from the device's, leaving out any it can't.
	values func(c capabilities) contract.State
	// write is what to write to set a capability to v, in order. None means there's nothing to do.
	write func(capability string, v any, c capabilities) ([]mWrite, error)
}

// magellanModels is every kind of Magellan device the plugin serves, by modelId. A model that isn't
// here isn't served, and is logged once.
var magellanModels = map[string]mModel{
	// Atlantic, Thermor and Sauter towel radiators: the Homey app's towel-rack models, and 1560, a
	// Thermor Riva 5.
	"1381": mTowelDryer, "1382": mTowelDryer, "1386": mTowelDryer, "1543": mTowelDryer, "1546": mTowelDryer,
	"1547": mTowelDryer, "1551": mTowelDryer, "1560": mTowelDryer, "1622": mTowelDryer,
}

// A towel radiator's capabilities on Magellan.
const (
	// mMode is 0 off and 4 heating.
	mMode = 7
	// mTarget is the temperature it's aiming for, in its programme too, where a write is an
	// override until the programme's next step.
	mTarget = 40
	// mRoom is its room sensor.
	mRoom = 117
	// mMinimum and mMaximum are the setpoints it takes.
	mMinimum = 160
	mMaximum = 161
	// mProgramme is 1 while it follows its weekly programme, and 0 in manual.
	mProgramme = 184
)

const (
	mModeOff     = "0"
	mModeHeating = "4"
	mManual      = "0"
)

// mTowelDryer is a towel radiator on Magellan, meaning what AtlanticElectricalTowelDryer does on
// Overkiz: off only when its mode is off, and on from off is manual. In its programme it's on, and
// stays in its programme, and setpoint is the temperature it's aiming for.
var mTowelDryer = mModel{
	capabilities: []string{contract.On, contract.Setpoint, contract.Temperature},
	values: func(c capabilities) contract.State {
		out := contract.State{}
		if mode, ok := c[mMode]; ok {
			out[contract.On] = strings.TrimSpace(mode) != mModeOff
		}
		if f, ok := c.number(mTarget); ok && f >= 0 && f <= 50 {
			out[contract.Setpoint] = f
		}
		if f, ok := c.number(mRoom); ok && f > -273.15 && f < 1000 {
			out[contract.Temperature] = f
		}
		return out
	},
	write: func(capability string, v any, c capabilities) ([]mWrite, error) {
		switch capability {
		case contract.On:
			on, ok := v.(bool)
			mode, known := c[mMode]
			switch {
			case !ok:
				return nil, fmt.Errorf("on %v is not a bool", v)
			case !on:
				return []mWrite{{mMode, mModeOff}}, nil
			case known && strings.TrimSpace(mode) != mModeOff:
				return nil, nil
			}
			return []mWrite{{mMode, mModeHeating}, {mProgramme, mManual}}, nil
		case contract.Setpoint:
			lowest, ok := c.number(mMinimum)
			if !ok {
				lowest = 0
			}
			highest, ok := c.number(mMaximum)
			if !ok {
				highest = 50
			}
			f, err := within(capability, v, lowest, highest)
			if err != nil {
				return nil, err
			}
			return []mWrite{{mTarget, strconv.FormatFloat(f, 'f', -1, 64)}}, nil
		}
		return nil, fmt.Errorf("%s can't be set", capability)
	},
}

// mDeviceState is one Magellan device the plugin serves.
type mDeviceState struct {
	id       string
	deviceID string
	model    mModel
	info     contract.DiscoveredDevice
	online   bool
	caps     capabilities
	values   contract.State
}

// magellanIDPrefix starts every Magellan device's ID, magellan.<deviceId>. No Overkiz protocol is
// named magellan, so the two clouds' IDs never meet.
const magellanIDPrefix = "magellan."

// magellan follows a Cozytouch account's devices on Magellan.
type magellan struct {
	follower
	cozytouch Cozytouch

	// kick asks for an early read, after a write.
	kick chan struct{}

	mu sync.Mutex
	// client is the session's, nil while there's none: every device is then offline.
	client  *magellanClient
	devices map[string]*mDeviceState
	// running are the executions under way, by ID.
	running map[string]mExecution
	// queued are the writes waiting for their device's execution to end, by device, in order, and
	// sending the devices a write is being sent to.
	queued  map[string][]mQueued
	sending map[string]bool
	// skipped are the models not served, each logged once.
	skipped map[string]bool
}

func newMagellan(c Cozytouch, t timing, log *slog.Logger) *magellan {
	return &magellan{
		account: c, timing: t, log: log, name: "Magellan",
		cozytouch: c, kick: make(chan struct{}, 1),
		devices: map[string]*mDeviceState{}, running: map[string]mExecution{},
		queued: map[string][]mQueued{}, sending: map[string]bool{}, skipped: map[string]bool{},
	}
}

// serve reports to out, and follows the account's devices on Magellan until ctx ends.
func (m *magellan) serve(ctx context.Context, out reporter) {
	m.out = out
	m.follow(ctx, m)
}

func (m *magellan) session(ctx context.Context, connected func()) (err error) {
	httpc := &http.Client{Timeout: m.timing.request, Transport: m.cozytouch.transport}
	defer httpc.CloseIdleConnections()
	// A token both clouds have used may have ended, so one Magellan refuses as it reads the setup is
	// dropped, and a new one asked for once: only a new token refused is a refused login.
	var (
		c     *magellanClient
		token string
	)
	for fresh := false; ; fresh = true {
		var failed error
		if token, failed = m.cozytouch.token(ctx); failed != nil {
			return failed
		}
		c = newMagellanClient(httpc, cmp.Or(m.cozytouch.atlanticAPI, atlanticAPI), token)
		failed = m.read(ctx, c, true)
		if failed == nil {
			break
		}
		if !refusedToken(failed) {
			return failed
		}
		m.cozytouch.forget(token)
		if fresh {
			return failed
		}
	}
	defer func() {
		// Atlantic's token has ended, so the next login asks for another.
		if refusedToken(err) {
			m.cozytouch.forget(token)
		}
	}()
	m.setProblem(ctx, "")
	m.log.Info("Connected to Magellan", "account", m.describe())
	connected()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.interval()):
		case <-m.kick:
			// A write was taken: see how it's going, then at the busy interval until it's done.
		}
		done, err := m.executions(ctx, c)
		if err != nil {
			return err
		}
		if done {
			if err := m.read(ctx, c, false); err != nil {
				return err
			}
		}
	}
}

func (m *magellan) describe() string { return m.account.describe() }

// refusedToken is whether err is a cloud refusing Atlantic's token.
func refusedToken(err error) bool {
	var e *apiError
	return errors.As(err, &e) && e.Status == http.StatusUnauthorized
}

// interval is how long until the next read: sooner while an execution runs.
func (m *magellan) interval() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.running) > 0 || len(m.queued) > 0 {
		return m.timing.busy
	}
	return m.timing.read
}

// executions asks how the executions under way are going, and says whether the setup should be read:
// when none is left running.
func (m *magellan) executions(ctx context.Context, c *magellanClient) (bool, error) {
	m.mu.Lock()
	ids := slices.Sorted(maps.Keys(m.running))
	m.mu.Unlock()
	now := time.Now()
	for _, id := range ids {
		state, err := c.execution(ctx, id)
		var e *apiError
		switch {
		case errors.As(err, &e) && (classify(err) == refused || classify(err) == limited):
			return false, err
		case err != nil && !errors.As(err, &e):
			return false, err
		case err != nil:
			// The cloud doesn't know it any more: its setup says what happened.
			m.finish(id)
		case state == execPending || state == execInProgress:
			m.mu.Lock()
			e, ok := m.running[id]
			expired := ok && now.Sub(e.began) > m.timing.execution
			if expired {
				delete(m.running, id)
			}
			m.mu.Unlock()
			if expired {
				m.log.Warn("Stopped waiting for Magellan execution", "exec", id, "device", e.id,
					"capability", e.set, "value", e.value, "after", m.timing.execution.String())
			}
		case state == execDone:
			m.finish(id)
		case state == execRefused:
			e := m.finish(id)
			m.log.Warn("Magellan execution failed", "exec", id, "device", e.id, "capability", e.set, "value", e.value)
		default:
			e := m.finish(id)
			m.log.Warn("Magellan execution ended in a state the plugin doesn't know", "exec", id, "state", state,
				"device", e.id, "capability", e.set, "value", e.value)
		}
	}
	// What waited for an execution that has ended goes now.
	m.mu.Lock()
	waiting := slices.Sorted(maps.Keys(m.queued))
	m.mu.Unlock()
	for _, id := range waiting {
		send, cancel := context.WithTimeout(ctx, m.timing.set)
		err := m.send(send, c, id)
		cancel()
		if err != nil {
			var e *apiError
			if errors.As(err, &e) && (classify(err) == refused || classify(err) == limited) {
				return false, err
			}
			m.log.Error("Could not send Magellan write", "device", id, "err", err)
			// The setup, read once nothing runs, says where the device is.
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.running) == 0 && len(m.queued) == 0, nil
}

// finish forgets an execution that has ended, and returns it.
func (m *magellan) finish(id string) mExecution {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.running[id]
	delete(m.running, id)
	return e
}

// send sends a device's queued writes in order, while none of its executions runs, and returns once
// one is under way or none is left.
func (m *magellan) send(ctx context.Context, c *magellanClient, id string) error {
	for {
		m.mu.Lock()
		queue := m.queued[id]
		if len(queue) == 0 || m.sending[id] || m.runsLocked(id) {
			m.mu.Unlock()
			return nil
		}
		next := queue[0]
		if len(queue) == 1 {
			delete(m.queued, id)
		} else {
			m.queued[id] = queue[1:]
		}
		m.sending[id] = true
		m.mu.Unlock()

		exec, err := c.write(ctx, next.deviceID, next.capability, next.value)
		m.mu.Lock()
		delete(m.sending, id)
		var dropped []mQueued
		switch {
		case err != nil:
			// What waited on it was decided from it, so it doesn't go either.
			dropped = m.queued[id]
			delete(m.queued, id)
		case exec != "":
			m.running[exec] = mExecution{mQueued: next, began: time.Now()}
		}
		if dev := m.devices[id]; err == nil && dev != nil {
			// What's written is what the next write decides from, until the setup is read again.
			dev.caps[next.capability] = next.value
		}
		m.mu.Unlock()
		if err != nil {
			m.dropped(dropped, "an earlier write failed")
			return fmt.Errorf("could not set %s to %s on %s: %w", next.set, next.value, id, err)
		}
		if exec == "" {
			// With no execution to wait for, the next waits for the next look at the executions.
			return nil
		}
	}
}

// dropped logs writes that were taken, and won't be sent: the setup read that follows says where
// the devices are.
func (m *magellan) dropped(writes []mQueued, why string) {
	for _, q := range writes {
		m.log.Warn("Dropped Magellan write", "device", q.id, "capability", q.set, "value", q.value, "reason", why)
	}
}

// runsLocked is whether one of a device's executions is under way.
func (m *magellan) runsLocked(id string) bool {
	for _, e := range m.running {
		if e.id == id {
			return true
		}
	}
	return false
}

// read reads the setup, and reports what changed.
func (m *magellan) read(ctx context.Context, c *magellanClient, all bool) error {
	setups, err := c.setup(ctx)
	if err != nil {
		return err
	}
	// A setup read is a session that can write, and it is one before its devices are reported
	// online, so a write that follows at once is sent rather than refused as unreachable.
	m.mu.Lock()
	m.client = c
	m.mu.Unlock()
	return m.report(ctx, m.served(setups), all)
}

// served is every device in the setups that the plugin serves.
func (m *magellan) served(setups []mSetup) map[string]*mDeviceState {
	devices := map[string]*mDeviceState{}
	for _, s := range setups {
		zones := map[flexID]string{}
		for _, z := range s.Zones {
			zones[z.ID] = strings.TrimSpace(z.Name)
		}
		alive := map[flexID]bool{}
		for _, g := range s.Gateways {
			alive[g.ID] = g.IsAlive == nil || *g.IsAlive
		}
		for _, d := range s.Devices {
			model, ok := magellanModels[string(d.ModelID)]
			if !ok {
				m.skip(d)
				continue
			}
			id := magellanIDPrefix + string(d.DeviceID)
			if !contract.ValidID(id) || d.DeviceID == "" {
				m.log.Warn("Skipped Magellan device whose ID makes no Oikade ID", "deviceId", string(d.DeviceID))
				continue
			}
			if _, ok := devices[id]; ok {
				m.log.Warn("Skipped Magellan device listed twice", "device", id)
				continue
			}
			caps := capabilities{}
			for _, cp := range d.Capabilities {
				if n, err := strconv.Atoi(string(cp.ID)); err == nil {
					caps[n] = string(cp.Value)
				}
			}
			online := d.IsAvailable == nil || *d.IsAvailable
			if a, ok := alive[d.GatewayID]; ok && !a {
				online = false
			}
			name := strings.TrimSpace(d.Name)
			devices[id] = &mDeviceState{
				id: id, deviceID: string(d.DeviceID), model: model, online: online, caps: caps, values: model.values(caps),
				info: contract.DiscoveredDevice{
					ID: id, Manufacturer: m.cozytouch.manufacturer(), Model: name,
					Capabilities: slices.Clone(model.capabilities), Name: cmp.Or(zones[d.ZoneID], name),
				},
			}
		}
	}
	return devices
}

// report reports devices, as read: all of it if all is set, or when the devices themselves have
// changed, and otherwise only the values and availability that did.
func (m *magellan) report(ctx context.Context, devices map[string]*mDeviceState, all bool) error {
	type report struct {
		id     string
		online *bool
		values contract.State
	}
	var reports []report
	m.mu.Lock()
	all = all || !sameDevices(m.devices, devices)
	for _, id := range slices.Sorted(maps.Keys(devices)) {
		d, was := devices[id], m.devices[id]
		r := report{id: id, values: contract.State{}}
		switch {
		case all:
			r.online, r.values = &d.online, maps.Clone(d.values)
		default:
			if d.online != was.online {
				r.online = &d.online
			}
			for c, v := range d.values {
				if old, had := was.values[c]; !had || old != v {
					r.values[c] = v
				}
			}
		}
		if r.online != nil || len(r.values) > 0 {
			reports = append(reports, r)
		}
	}
	m.devices = devices
	discovered, found := m.reportsLocked()
	m.mu.Unlock()

	if all {
		if err := m.out.SetDiscovered(ctx, discovered); err != nil {
			return reporting(err)
		}
		if err := m.out.SetDevices(ctx, found, 0); err != nil {
			return reporting(err)
		}
	}
	for _, r := range reports {
		if r.online != nil {
			if err := m.out.SetAvailability(ctx, r.id, *r.online); err != nil {
				return reporting(err)
			}
		}
		if len(r.values) > 0 {
			if err := m.out.ReportState(ctx, r.id, r.values); err != nil {
				return reporting(err)
			}
		}
	}
	return nil
}

// sameDevices is whether two reads found the same devices, with the same info.
func sameDevices(a, b map[string]*mDeviceState) bool {
	return maps.EqualFunc(a, b, func(x, y *mDeviceState) bool { return reflect.DeepEqual(x.info, y.info) })
}

func (m *magellan) reportsLocked() ([]contract.DiscoveredDevice, []contract.DeviceReport) {
	discovered := make([]contract.DiscoveredDevice, 0, len(m.devices))
	found := make([]contract.DeviceReport, 0, len(m.devices))
	for _, id := range slices.Sorted(maps.Keys(m.devices)) {
		d := m.devices[id]
		discovered = append(discovered, d.info)
		found = append(found, contract.DeviceReport{
			ID: id, Found: true, Manufacturer: d.info.Manufacturer, Model: d.info.Model, Capabilities: d.info.Capabilities,
		})
	}
	return discovered, found
}

// skip logs a model that isn't served, once.
func (m *magellan) skip(d mDevice) {
	m.mu.Lock()
	seen := m.skipped[string(d.ModelID)]
	m.skipped[string(d.ModelID)] = true
	m.mu.Unlock()
	if !seen {
		m.log.Info("Skipped Magellan device of a kind the plugin doesn't serve", "model", string(d.ModelID),
			"product", string(d.ProductID), "family", d.ModelFamily, "name", strings.TrimSpace(d.Name))
	}
}

func (m *magellan) pause() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.client = nil
}

func (m *magellan) lost(ctx context.Context) {
	m.mu.Lock()
	m.client = nil
	clear(m.running)
	var dropped []mQueued
	for _, id := range slices.Sorted(maps.Keys(m.queued)) {
		dropped = append(dropped, m.queued[id]...)
	}
	clear(m.queued)
	ids := slices.Sorted(maps.Keys(m.devices))
	m.mu.Unlock()
	m.dropped(dropped, "the session ended")
	for _, id := range ids {
		m.check(m.out.SetAvailability(ctx, id, false), "Could not report Magellan device offline", id)
	}
	// Nil, not empty: an empty list would say every device was removed from the app.
	m.check(m.out.SetDiscovered(ctx, nil), "Could not report Magellan devices", "")
}

func (m *magellan) absent(ctx context.Context) {
	m.mu.Lock()
	m.devices = map[string]*mDeviceState{}
	discovered, found := m.reportsLocked()
	m.mu.Unlock()
	m.check(m.out.SetDiscovered(ctx, discovered), "Could not report Magellan devices", "")
	m.check(m.out.SetDevices(ctx, found, 0), "Could not report Magellan devices", "")
}

// set writes a command's capabilities, and returns once Magellan has taken what can go now, before
// the device has acted on it. A write waits for the device's last execution to end, and is sent by
// the session's next look at its executions. It fails at once while there's no session.
func (m *magellan) set(ctx context.Context, cmd contract.SetCommand) error {
	m.mu.Lock()
	c, dev := m.client, m.devices[cmd.ID]
	var (
		writes   []mWrite
		err      error
		deviceID string
	)
	if dev != nil {
		// A write decides from what was written and what's waiting, as a quick off then on does.
		caps := maps.Clone(dev.caps)
		for _, q := range m.queued[cmd.ID] {
			caps[q.capability] = q.value
		}
		writes, err = dev.model.write(cmd.Capability, cmd.Value, caps)
		deviceID = dev.deviceID
	}
	if dev != nil && c != nil && err == nil {
		// A newer write to a capability replaces one still waiting.
		queue := m.queued[cmd.ID]
		for _, w := range writes {
			queue = slices.DeleteFunc(queue, func(q mQueued) bool { return q.capability == w.capability })
			queue = append(queue, mQueued{id: cmd.ID, deviceID: deviceID, set: cmd.Capability, mWrite: w})
		}
		if len(queue) > 0 {
			m.queued[cmd.ID] = queue
		}
	}
	m.mu.Unlock()
	switch {
	case dev == nil:
		return fmt.Errorf("no Magellan device %s", cmd.ID)
	case c == nil:
		return fmt.Errorf("can't reach %s", m.describe())
	case err != nil:
		return fmt.Errorf("could not set %s on %s: %w", cmd.Capability, cmd.ID, err)
	case len(writes) == 0:
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, m.timing.set)
	defer cancel()
	defer func() {
		select {
		case m.kick <- struct{}{}:
		default:
		}
	}()
	// What can go now goes, and the rest once the execution before it has ended.
	return m.send(ctx, c, cmd.ID)
}

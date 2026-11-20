package host

import (
	"encoding/json"
	"net"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/nodetest"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/registry"
)

const (
	turnOn  = 0x01
	turnOff = 0x00

	// labelAttribute and uniqueIDAttribute are in Bridged Device Basic Information.
	labelAttribute     = 0x05
	reachableAttribute = 0x11
	uniqueIDAttribute  = 0x12
	// nodeLabelAttribute is in Basic Information.
	nodeLabelAttribute = 0x05
)

// controller is a Matter controller paired with a rig's host.
type controller struct {
	t     *testing.T
	r     *rig
	port  uint16
	cm    *nodetest.Commissioner
	admin *im.Client
}

// freeUDP picks a UDP port for a host's Matter bridge. The host is another process, so the port is
// let go before the host binds it, and another test can take it first: serveMatter recovers.
func freeUDP(t *testing.T) uint16 {
	t.Helper()
	pc, err := net.ListenPacket("udp", "[::]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	return uint16(pc.LocalAddr().(*net.UDPAddr).Port)
}

func matterFlag(port uint16) string { return "--matter-port=" + strconv.Itoa(int(port)) }

// serveMatter launches the host with launch, which passes on the Matter flag it's given, and waits
// for its bridge to serve. If something took the port first, it stops the host and launches it
// again on another. It returns the port the bridge serves on, as oikade matter says.
func (r *rig) serveMatter(port uint16, launch func(flag string)) uint16 {
	r.t.Helper()
	for range 5 {
		launch(matterFlag(port))
		var m api.Matter
		r.until("the bridge to serve", func() bool {
			m = r.matter()
			return m.Serving || portTaken(m.Problem)
		})
		if m.Serving {
			return m.Port
		}
		r.t.Logf("Matter port %d was taken: launching the host again on another", port)
		r.stop()
		port = freeUDP(r.t)
	}
	r.t.Fatalf("every Matter port tried was taken. The log:\n%s", r.dump())
	return 0
}

// portTaken says a bridge is down because its port is taken.
func portTaken(problem string) bool { return strings.Contains(problem, "address already in use") }

// relaunch launches the host again with launch, as serveMatter does, and has the controller reach
// it on the port it serves on.
func (c *controller) relaunch(launch func(flag string)) {
	c.t.Helper()
	c.port = c.r.serveMatter(c.port, launch)
	c.admin = c.cm.CASE(nodetest.Addr(c.port))
}

// launchAttached launches the host with args, and waits for its process to connect.
func (r *rig) launchAttached(args ...string) {
	r.t.Helper()
	r.launch(args...)
	r.waitFor(func(l line) bool { return l.is(attached) })
}

// matter asks the host about its bridge.
func (r *rig) matter() api.Matter {
	r.t.Helper()
	st := r.status("matter")
	if st.Matter == nil {
		r.t.Fatal("status matter --json has no bridge")
	}
	return *st.Matter
}

func (r *rig) until(what string, cond func() bool) {
	r.t.Helper()
	r.untilWithin(waitLimit, what, cond)
}

// untilWithin is until with a limit of its own, for a test that something happens quickly.
func (r *rig) untilWithin(d time.Duration, what string, cond func() bool) {
	r.t.Helper()
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	r.t.Fatalf("timed out waiting for %s. The log:\n%s", what, r.dump())
}

// commissioned starts a host and pairs a controller with it.
func commissioned(t *testing.T) *controller {
	t.Helper()
	r := prepare(t)
	port := r.serveMatter(freeUDP(t), func(flag string) { r.launchAttached(flag) })
	r.until("the commissioning window", func() bool { return r.matter().Window })
	c := &controller{t: t, r: r, port: port, cm: nodetest.New(t)}
	c.admin = c.cm.Commission(nodetest.Addr(port), r.matter().Passcode)
	return c
}

// endpoint is a device's endpoint on the bridge.
func (c *controller) endpoint(name string) uint16 {
	c.t.Helper()
	return deviceOf(c.t, c.r.devices(), name).Endpoint
}

func (c *controller) read(endpoint uint16, cluster, attribute uint32) (any, bool) {
	c.t.Helper()
	path := im.AttributePath{Endpoint: endpoint, Cluster: cluster, Attribute: attribute}
	reports, err := c.admin.Read(nodetest.Context(c.t), path)
	if err != nil {
		c.t.Fatalf("reading endpoint %d: %v", endpoint, err)
	}
	if len(reports) != 1 || reports[0].Data == nil {
		return nil, false
	}
	v, err := im.DecodeValue(reports[0].Data)
	if err != nil {
		c.t.Fatal(err)
	}
	return v, true
}

func (c *controller) isOn(endpoint uint16) bool {
	c.t.Helper()
	v, ok := c.read(endpoint, clusters.IDOnOff, 0)
	if !ok {
		c.t.Fatalf("endpoint %d has no OnOff", endpoint)
	}
	return v.(bool)
}

func (c *controller) info(endpoint uint16, attribute uint32) any {
	c.t.Helper()
	v, ok := c.read(endpoint, clusters.IDBridgedDeviceBasicInformation, attribute)
	if !ok {
		c.t.Fatalf("endpoint %d has no attribute %#x", endpoint, attribute)
	}
	return v
}

func (c *controller) command(endpoint uint16, command uint32) {
	c.t.Helper()
	path := im.CommandPath{Endpoint: endpoint, Cluster: clusters.IDOnOff, Command: command}
	res, err := c.admin.Invoke(nodetest.Context(c.t), path, func(*tlv.Writer) {}, false, 0)
	if err != nil || res.Status != im.StatusSuccess {
		c.t.Fatalf("command %#x on endpoint %d: %+v, %v", command, endpoint, res, err)
	}
}

// reaches proves the controller drives a device, and hears it back.
func (c *controller) reaches(name string) { c.reachesHardware(name, name) }

// reachesHardware is reaches, for a device whose driver knows it by another name.
func (c *controller) reachesHardware(name, id string) {
	c.t.Helper()
	endpoint := c.endpoint(name)
	want := !c.isOn(endpoint)
	command := uint32(turnOff)
	if want {
		command = turnOn
	}
	c.command(endpoint, command)
	c.r.until(name+" to change", func() bool { return onOf(c.t, c.r.status(), name).Value == want })
	c.r.send(id, "on", !want)
	c.r.until("the controller to hear "+name, func() bool { return c.isOn(endpoint) != want })
}

func TestAControllerPairsWithTheHostAndDrivesADevice(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	m := c.r.matter()
	if !m.Serving || m.Window || len(m.Fabrics) != 1 || m.ManualCode == "" || !strings.HasPrefix(m.QRCode, "MT:") {
		t.Errorf("after pairing: %+v", m)
	}
	c.reaches("fan")
	if v := c.info(c.endpoint("fan"), labelAttribute); v != "fan" {
		t.Errorf("label %v", v)
	}
	c.r.stop()
	if c.r.count("Stopped host") != 1 {
		t.Errorf("the host didn't stop cleanly:\n%s", c.r.dump())
	}
}

// Once the bridge is paired, oikade matter open opens a window with a code of its own, and a second
// controller pairs with it.
func TestOikadeMatterOpenPairsASecondController(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	if out := string(c.r.stdout(nil, "matter")); !strings.Contains(out, "run oikade matter open") {
		t.Errorf("oikade matter once paired:\n%s", out)
	}
	out := string(c.r.stdout(nil, "matter", "open", "--for", "3m"))
	if !strings.Contains(out, "Opened the Matter bridge's window until") ||
		!strings.Contains(out, "pair nothing once it closes") || strings.Contains(out, "█") {
		t.Errorf("oikade matter open:\n%s", out)
	}
	before := time.Now()
	var w api.MatterWindow
	if err := json.Unmarshal(c.r.stdout(nil, "matter", "open", "--json"), &w); err != nil {
		t.Fatal(err)
	}
	if !w.Fresh || w.Passcode == c.r.matter().Passcode || !strings.HasPrefix(w.QRCode, "MT:") || len(w.ManualCode) != 11 ||
		w.Expires.Before(before.Add(14*time.Minute)) || w.Expires.After(time.Now().Add(15*time.Minute)) {
		t.Fatalf("oikade matter open --json: %+v", w)
	}
	if m := c.r.matter(); !m.Window {
		t.Errorf("no window: %+v", m)
	}
	nodetest.New(t).Commission(nodetest.Addr(c.port), w.Passcode)
	c.r.until("the second controller, and the window closed", func() bool {
		m := c.r.matter()
		return len(m.Fabrics) == 2 && !m.Window
	})
	c.reaches("fan")
}

func TestAControllerOutlastsYourProcessAndADeploy(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	c.reaches("fan")
	old := c.r.pid()
	if err := syscall.Kill(old, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	// Frozen, your process does nothing at all, and the same session still works.
	c.reaches("siren")
	if err := syscall.Kill(old, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	c.r.restarted(old)
	c.reaches("trap-light")

	c.r.mustDeploy("same")
	c.reaches("probe-out")
}

func TestPairingSurvivesTheHostDying(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	before := c.r.matter()
	endpoint := c.endpoint("fan")
	unique := c.info(endpoint, uniqueIDAttribute)

	// A hard kill straight after pairing: what commissioning was told is on disk had better be.
	if err := c.r.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	c.r.died()
	c.relaunch(func(flag string) { c.r.launchAttached(flag) })

	var after api.Matter
	c.r.until("the fabric", func() bool {
		after = c.r.matter()
		return len(after.Fabrics) == 1
	})
	if after.Window || after.ManualCode != before.ManualCode || after.QRCode != before.QRCode {
		t.Errorf("before %+v, after %+v", before, after)
	}
	if got := c.endpoint("fan"); got != endpoint {
		t.Errorf("fan moved from endpoint %d to %d", endpoint, got)
	}
	if got := c.info(endpoint, uniqueIDAttribute); got != unique {
		t.Errorf("unique ID %v became %v", unique, got)
	}
	c.reaches("fan")
}

func TestANewDeviceAppearsOnTheBridgeAndGoes(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	c.r.pair("hall-lamp", "hall-lamp")
	endpoint := c.endpoint("hall-lamp")
	c.r.until("the new device on the bridge", func() bool {
		_, ok := c.read(endpoint, clusters.IDOnOff, 0)
		return ok
	})
	c.reaches("hall-lamp")

	c.r.stdout(nil, "forget", "hall-lamp")
	c.r.until("the device to leave the bridge", func() bool {
		_, ok := c.read(endpoint, clusters.IDOnOff, 0)
		return !ok
	})
}

// The home names the bridge, and a deploy renames it in place: the controller stays paired, and a
// restart keeps the name before your process connects.
func TestADeployRenamesTheBridge(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	if got := c.nodeLabel(); got != "Oikade" {
		t.Fatalf("a new bridge is named %v", got)
	}
	c.r.mustDeploy("cottage")
	c.r.until("the new name", func() bool { return c.nodeLabel() == "Cottage" })
	c.reaches("fan")

	c.r.stop()
	c.relaunch(func(flag string) { c.r.launchAttached(flag) })
	if got := c.nodeLabel(); got != "Cottage" {
		t.Errorf("after a restart, the bridge is named %v", got)
	}
}

// nodeLabel is the bridge's own name, in the root's Basic Information.
func (c *controller) nodeLabel() any {
	c.t.Helper()
	v, _ := c.read(0, clusters.IDBasicInformation, nodeLabelAttribute)
	return v
}

func TestAControllerGoesThroughArbitration(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	endpoint := c.endpoint("claimed")
	c.command(endpoint, turnOff)
	c.r.until("the Matter claim", func() bool {
		s := onOf(t, c.r.status(), "claimed")
		return s.Value == false && winner(s) == "matter"
	})
}

func TestASafetyClaimRefusesAController(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	endpoint := c.endpoint("valve")
	c.r.change("leak-sensor", "open", true)
	c.r.waitFor(func(l line) bool { return l.is(wrote, "device", "valve", "value", false) })
	c.r.until("the controller to see the valve shut", func() bool { return !c.isOn(endpoint) })

	c.command(endpoint, turnOn)
	c.r.waitFor(func(l line) bool {
		return l.is("Refused Matter write", "capability", "on", "claim", "case/valve/on/leak-shutoff", "claimLevel", "Safety")
	})
	if n := c.r.count("Failed to set capability from Matter"); n != 0 {
		t.Errorf("a refusal was logged as a failure %d times", n)
	}
	c.r.until("the controller to see it refused", func() bool { return !c.isOn(endpoint) })
	if n := c.r.count(wrote, "device", "valve", "value", true); n != 0 {
		t.Errorf("the valve opened %d times", n)
	}
}

func TestATakenPortLeavesTheHomeRunning(t *testing.T) {
	t.Parallel()
	pc, err := net.ListenPacket("udp", "[::]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	r := start(t, matterFlag(uint16(pc.LocalAddr().(*net.UDPAddr).Port)))
	r.until("the bridge to give up", func() bool { return r.count("Matter bridge stopped") > 0 })
	if m := r.matter(); m.Serving {
		t.Errorf("it says it's serving: %+v", m)
	}
	r.change("leak-sensor", "open", true)
	r.waitFor(func(l line) bool { return l.is(wrote, "device", "valve", "value", false) })
	out := string(r.stdout(nil, "matter"))
	if !strings.Contains(out, "down") || strings.Contains(out, "window") {
		t.Errorf("oikade matter says: %s", out)
	}
}

func TestAnInterfaceThatIsntThereLeavesTheHomeRunning(t *testing.T) {
	t.Parallel()
	r := start(t, matterFlag(freeUDP(t)), "--matter-interfaces=nothing0")
	r.until("the bridge to give up", func() bool { return r.count("Matter bridge stopped") > 0 })
	r.change("leak-sensor", "open", true)
	r.waitFor(func(l line) bool { return l.is(wrote, "device", "valve", "value", false) })
}

func TestMatterOffIsSaid(t *testing.T) {
	t.Parallel()
	r := start(t)
	if stderr, err := r.oikade(nil, "matter"); err == nil || !strings.Contains(stderr, "Matter turned off") {
		t.Errorf("%v: %s", err, stderr)
	}
}

// TestARepeatedWriteIsRefusedOnceSafetyHasShutAValve is a controller asking twice: its first write
// went through, then the device changed, and what it sent then says nothing about what's there now.
func TestARepeatedWriteIsRefusedOnceSafetyHasShutAValve(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	endpoint := c.endpoint("valve")
	c.command(endpoint, turnOff)
	c.r.wrote("valve", false)
	c.command(endpoint, turnOn)
	c.r.wrote("valve", true)
	c.r.until("the controller to see it open", func() bool { return c.isOn(endpoint) })

	c.r.change("leak-sensor", "open", true)
	c.r.until("the controller to see the valve shut", func() bool { return !c.isOn(endpoint) })
	c.command(endpoint, turnOn)
	c.r.until("the write to be refused", func() bool { return c.r.count("Refused Matter write") > 0 })
	c.r.until("the controller to see it shut", func() bool { return !c.isOn(endpoint) })
}

func TestUnreadableMatterStateLeavesTheHomeRunning(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"identity", "fabrics"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			r := prepare(t)
			s := r.openStore()
			if err := s.Namespace("matter").Put(key, []byte("not json")); err != nil {
				t.Fatal(err)
			}
			_ = s.Close()
			r.launch(matterFlag(freeUDP(t)))
			r.waitFor(func(l line) bool { return l.is(attached) })

			r.change("leak-sensor", "open", true)
			r.wrote("valve", false)
			if m := r.matter(); m.Serving || m.Problem == "" || len(m.Fabrics) != 0 {
				t.Errorf("oikade status matter --json says %+v", m)
			}
			if out := string(r.stdout(nil, "matter")); !strings.Contains(out, "down: ") {
				t.Errorf("oikade matter says: %s", out)
			}
			if r.count("Matter bridge stopped") != 1 {
				t.Errorf("the log doesn't say. The log:\n%s", r.dump())
			}
			r.stop()
			s = r.openStore()
			defer func() { _ = s.Close() }()
			if raw, _, _ := s.Namespace("matter").Get(key); string(raw) != "not json" {
				t.Errorf("it was replaced with %s", raw)
			}
		})
	}
}

// TestAnUnreadableClaimRecordHoldsItsDevice is a Safety claim that may be in a record nobody can read.
// Nothing writes the device, nothing writes over the record, and an operator can clear it.
func TestAnUnreadableClaimRecordHoldsItsDevice(t *testing.T) {
	t.Parallel()
	const record = "valve"
	r := prepare(t)
	s := r.openStore()
	if err := s.Namespace("controls").Put(record, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	port := freeUDP(t)
	r.launch(matterFlag(port))
	r.waitFor(func(l line) bool { return l.is(attached) })

	stderr, err := r.oikade(nil, "set", "valve", "on", "off")
	if err == nil || !strings.Contains(stderr, "unreadable") {
		t.Errorf("a person's write: %v: %s", err, stderr)
	}
	r.change("leak-sensor", "open", true)
	r.until("a rule's write to be refused", func() bool {
		return strings.Contains(r.dump(), "unreadable claim record holds valve")
	})
	if n := r.count(wrote, "device", "valve", "value", false); n != 0 {
		t.Errorf("the valve was written %d times", n)
	}
	stderr, err = r.oikade(nil, "forget", "valve")
	if err == nil || !strings.Contains(stderr, "--allow-safety") {
		t.Errorf("removing it: %v: %s", err, stderr)
	}
	r.stop()
	s = r.openStore()
	if raw, _, _ := s.Namespace("controls").Get(record); string(raw) != "not json" {
		t.Errorf("it was replaced with %s", raw)
	}
	_ = s.Close()

	r.launch(matterFlag(port))
	r.waitFor(func(l line) bool { return l.is(attached) })
	r.stdout(nil, "forget", "valve", "--allow-safety")
	r.stop()
	s = r.openStore()
	defer func() { _ = s.Close() }()
	if raw, ok, _ := s.Namespace("controls").Get(record); ok {
		t.Errorf("the record stayed: %s", raw)
	}
}

func TestTheBridgeReportsWhatControllersDo(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	c.reaches("fan")
	r := c.r
	for series, want := range map[string]string{
		"oikade_matter_fabrics":                      "1",
		"oikade_matter_serving":                      "1",
		"oikade_matter_subscriptions":                "0",
		`oikade_matter_panics_total{site="handler"}`: "0",
	} {
		if got := r.metric(series); got != want {
			t.Errorf("%s: %q, want %s", series, got, want)
		}
	}
	if got, _ := strconv.Atoi(r.metric("oikade_matter_sessions")); got < 1 {
		t.Errorf("oikade_matter_sessions: %d, want a session at least", got)
	}
	// Commissioning is invokes too, so the fan's command is one of several.
	on := `oikade_matter_interactions_total{cluster="on_off",interaction="invoke",result="success"}`
	if got := r.metric(on); got != "1" {
		t.Errorf("%s: %q, want 1", on, got)
	}
	commissioning := `oikade_matter_interactions_total{` +
		`cluster="operational_credentials",interaction="invoke",result="success"}`
	if got := r.metric(commissioning); got == "" {
		t.Error("commissioning's commands aren't counted")
	}
	// The write is timed once its driver answers, which can be after the device reports.
	r.until("the write to be timed", func() bool { return r.metric("oikade_matter_write_duration_seconds_count") == "1" })

	// A command to an endpoint that isn't there is counted by what it was answered with.
	path := im.CommandPath{Endpoint: 0xFFF0, Cluster: clusters.IDOnOff, Command: turnOn}
	if _, err := c.admin.Invoke(nodetest.Context(t), path, func(*tlv.Writer) {}, false, 0); err != nil {
		t.Fatal(err)
	}
	refused := `oikade_matter_interactions_total{cluster="on_off",interaction="invoke",result="unsupported_endpoint"}`
	if got := r.metric(refused); got != "1" {
		t.Errorf("%s: %q, want 1", refused, got)
	}
}

func TestAHostWithMatterOffExportsNoMatterSeries(t *testing.T) {
	t.Parallel()
	r := start(t)
	if strings.Contains(r.metrics(), "oikade_matter_") {
		t.Errorf("Matter is off, and /metrics has:\n%s", r.metrics())
	}
}

// deviceTypes are the device types a controller reads from an endpoint's Descriptor.
func (c *controller) deviceTypes(endpoint uint16) []uint64 {
	c.t.Helper()
	v, ok := c.read(endpoint, clusters.IDDescriptor, 0)
	if !ok {
		return nil
	}
	var out []uint64
	for _, entry := range v.([]any) {
		out = append(out, entry.(map[uint8]any)[0].(uint64))
	}
	return out
}

func TestAControllerLocksASimulatedLock(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	endpoint := c.endpoint("back-door")
	if got := c.deviceTypes(endpoint); !slices.Contains(got, uint64(clusters.DeviceDoorLock.ID)) {
		t.Fatalf("the lock's device types are %#x, want a Door Lock", got)
	}
	state := func() any {
		v, _ := c.read(endpoint, clusters.IDDoorLock, 0x0000)
		return v
	}
	c.r.until("the controller to read the lock unlocked", func() bool {
		return state() == uint64(clusters.LockStateUnlocked)
	})
	for _, tc := range []struct {
		command uint32
		locked  bool
		state   uint64
	}{
		{clusters.CommandLockDoor, true, clusters.LockStateLocked},
		{clusters.CommandUnlockDoor, false, clusters.LockStateUnlocked},
	} {
		path := im.CommandPath{Endpoint: endpoint, Cluster: clusters.IDDoorLock, Command: tc.command}
		res, err := c.admin.Invoke(nodetest.Context(t), path, func(*tlv.Writer) {}, true, 1000)
		if err != nil || res.Status != im.StatusSuccess {
			t.Fatalf("command %#x: %+v, %v", tc.command, res, err)
		}
		c.r.until("the lock to follow", func() bool {
			return capabilityOf(t, c.r.status(), "back-door", "locked").Value == tc.locked
		})
		c.r.until("the controller to read it", func() bool { return state() == tc.state })
	}
}

// capabilityOf is one capability of a device in the host's status.
func capabilityOf(t *testing.T, st api.Status, device, name string) api.Capability {
	t.Helper()
	for _, d := range st.Devices {
		if d.Name != device {
			continue
		}
		for _, c := range d.Capabilities {
			if c.Name == name {
				return c
			}
		}
	}
	t.Fatalf("no %s on %s in %+v", name, device, st)
	return api.Capability{}
}

func TestARuleSetsAHeatersSetpointThatAControllerReads(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	endpoint := c.endpoint("study-heater")
	if got := c.deviceTypes(endpoint); !slices.Contains(got, uint64(clusters.DeviceThermostat.ID)) {
		t.Fatalf("the heater's device types are %#x, want a Thermostat", got)
	}
	setpoint := func() any {
		v, _ := c.read(endpoint, clusters.IDThermostat, 0x0012)
		return v
	}
	mode := func() any {
		v, _ := c.read(endpoint, clusters.IDThermostat, 0x001C)
		return v
	}
	c.r.until("the controller to read the heater's first setpoint", func() bool { return setpoint() == int64(2100) })

	c.r.change("study-go", "open", true)
	c.r.until("the controller to read the Rule's 18 °C", func() bool { return setpoint() == int64(1800) })
	if v := capabilityOf(t, c.r.status(), "study-heater", "setpoint").Value; v != 18.0 {
		t.Errorf("the host has the setpoint at %v", v)
	}

	write := func(attr uint32, v uint64) {
		t.Helper()
		path := im.AttributePath{Endpoint: endpoint, Cluster: clusters.IDThermostat, Attribute: attr}
		status, err := c.admin.Write(nodetest.Context(t), path, func(w *tlv.Writer, tag tlv.Tag) { w.PutUint(tag, v) })
		if err != nil || status != im.StatusSuccess {
			t.Fatalf("writing %#x = %d: %v, %v", attr, v, status, err)
		}
	}
	write(0x001C, clusters.SystemModeOff)
	c.r.until("the heater to turn off", func() bool { return onOf(t, c.r.status(), "study-heater").Value == false })
	write(0x001C, clusters.SystemModeHeat)
	c.r.until("the heater to turn on", func() bool { return onOf(t, c.r.status(), "study-heater").Value == true })
	if mode() != uint64(clusters.SystemModeHeat) {
		t.Errorf("SystemMode %v, want Heat", mode())
	}
}

// A controller changes a simulated air conditioner's mode, turns it off and sets its target, as the
// Home app would, and reads back what the host has.
func TestAControllerRunsASimulatedAirConditioner(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	endpoint := c.endpoint("bedroom-ac")
	if got := c.deviceTypes(endpoint); !slices.Contains(got, uint64(clusters.DeviceRoomAirConditioner.ID)) {
		t.Fatalf("the air conditioner's device types are %#x, want a Room Air Conditioner", got)
	}
	attr := func(id uint32) any {
		v, _ := c.read(endpoint, clusters.IDThermostat, id)
		return v
	}
	const cooling, heating, systemMode = 0x0011, 0x0012, 0x001C
	c.r.until("the controller to read it cooling to 24 °C", func() bool {
		return attr(systemMode) == uint64(clusters.SystemModeCool) && attr(cooling) == int64(2400)
	})
	write := func(id uint32, put func(w *tlv.Writer, tag tlv.Tag)) {
		t.Helper()
		path := im.AttributePath{Endpoint: endpoint, Cluster: clusters.IDThermostat, Attribute: id}
		status, err := c.admin.Write(nodetest.Context(t), path, put)
		if err != nil || status != im.StatusSuccess {
			t.Fatalf("writing %#x: %v, %v", id, status, err)
		}
	}
	value := func(name string) any { return capabilityOf(t, c.r.status(), "bedroom-ac", name).Value }

	write(systemMode, func(w *tlv.Writer, tag tlv.Tag) { w.PutUint(tag, clusters.SystemModeHeat) })
	c.r.until("the unit to heat", func() bool { return value("mode") == "heat" && value("on") == true })
	write(heating, func(w *tlv.Writer, tag tlv.Tag) { w.PutInt(tag, 2150) })
	c.r.until("the target to be 21.5 °C", func() bool { return value("setpoint") == 21.5 })
	write(systemMode, func(w *tlv.Writer, tag tlv.Tag) { w.PutUint(tag, clusters.SystemModeOff) })
	c.r.until("the unit to turn off", func() bool { return value("on") == false })
	if value("mode") != "heat" {
		t.Errorf("off changed the mode to %v", value("mode"))
	}
	c.r.until("the controller to read it off", func() bool { return attr(systemMode) == uint64(clusters.SystemModeOff) })
}

// commissionedPlugins is commissioned, on a host running the plugins, whose devices a home declares.
func commissionedPlugins(t *testing.T) *controller {
	t.Helper()
	r := preparePlugins(t)
	port := r.serveMatter(freeUDP(t), func(flag string) { r.launchedPlugins(flag) })
	r.until("the relays", func() bool {
		devices := r.devices().Devices
		return slices.ContainsFunc(devices, func(d registry.Device) bool { return d.ID == "relay" }) &&
			slices.ContainsFunc(devices, func(d registry.Device) bool { return d.ID == "valve" })
	})
	r.until("the commissioning window", func() bool { return r.matter().Window })
	c := &controller{t: t, r: r, port: port, cm: nodetest.New(t)}
	c.admin = c.cm.Commission(nodetest.Addr(port), r.matter().Passcode)
	return c
}

// restartPlugins restarts a commissionedPlugins host, on the version last deployed to it.
func (c *controller) restartPlugins() {
	c.t.Helper()
	c.r.stop()
	c.relaunch(func(flag string) {
		c.r.launchPlugins(flag)
		c.r.waitFor(func(l line) bool { return l.is(attached) })
	})
}

// drives turns a device on or off from the controller, and waits for the host to say it is.
func (c *controller) drives(name string, endpoint uint16, on bool) {
	c.t.Helper()
	command := uint32(turnOff)
	if on {
		command = turnOn
	}
	c.command(endpoint, command)
	c.r.until(name+" to change", func() bool { return onOf(c.t, c.r.status(), name).Value == on })
}

func TestADeclaredRenameKeepsTheBridgedDevice(t *testing.T) {
	t.Parallel()
	c := commissionedPlugins(t)
	endpoint := c.endpoint("valve")
	c.r.until("valve on the bridge", func() bool { return c.info(endpoint, labelAttribute) == "valve" })
	unique := c.info(endpoint, uniqueIDAttribute)

	c.r.mustDeploy("plugins-mains")
	c.r.until("the new label", func() bool { return c.info(endpoint, labelAttribute) == "mains-valve" })
	if got := c.info(endpoint, uniqueIDAttribute); got != unique {
		t.Errorf("unique ID %v became %v", unique, got)
	}
	c.drives("mains-valve", endpoint, false)

	// A restart builds the endpoint again, which is when a unique ID from the name would change.
	c.restartPlugins()
	if got := c.info(endpoint, uniqueIDAttribute); got != unique {
		t.Errorf("after a restart, unique ID %v became %v", unique, got)
	}
	if got := c.info(endpoint, labelAttribute); got != "mains-valve" {
		t.Errorf("after a restart, the label is %v", got)
	}
}

func TestADeclaredSwapOfHardwareReachesTheController(t *testing.T) {
	t.Parallel()
	c := commissionedPlugins(t)
	c.r.until("the spare relay", func() bool {
		return slices.ContainsFunc(c.r.devices().Devices, func(d registry.Device) bool { return d.ID == "spare-relay" })
	})
	c.r.mustDeploy("plugins-valve")
	endpoint := c.endpoint("valve")
	c.r.mustDeploy("plugins-swapped")
	if d := deviceOf(t, c.r.devices(), "valve"); d.ID != "spare-relay" || d.Endpoint != endpoint {
		t.Fatalf("got %+v, want the spare on endpoint %d", d, endpoint)
	}
	// The same endpoint drives the new hardware, and hears it.
	c.drives("valve", endpoint, true)
	c.r.until("the controller to hear it", func() bool { return c.isOn(endpoint) })
	c.drives("valve", endpoint, false)
	c.r.until("the controller to hear it", func() bool { return !c.isOn(endpoint) })
}

func TestAControllerReadsTheDeviceTypeYouDeclareThroughARestart(t *testing.T) {
	t.Parallel()
	c := commissionedPlugins(t)
	endpoint := c.endpoint("relay")
	plug, light := uint64(clusters.DeviceOnOffPlug.ID), uint64(clusters.DeviceOnOffLight.ID)
	c.r.until("the relay on the bridge", func() bool { return slices.Contains(c.deviceTypes(endpoint), plug) })

	// plugins-declared calls the relay porch-relay, an OnOffLight.
	c.r.mustDeploy("plugins-declared")
	c.r.until("the relay to be a light", func() bool { return slices.Contains(c.deviceTypes(endpoint), light) })
	if got := c.endpoint("porch-relay"); got != endpoint {
		t.Errorf("the relay moved from endpoint %d to %d", endpoint, got)
	}
	var listed []struct {
		Name        string   `json:"name"`
		DeviceTypes []string `json:"deviceTypes"`
	}
	if err := json.Unmarshal(c.r.stdout(nil, "list", "devices", "--json"), &listed); err != nil {
		t.Fatal(err)
	}
	for _, d := range listed {
		if d.Name == "porch-relay" && !slices.Equal(d.DeviceTypes, []string{"OnOffLight"}) {
			t.Errorf("oikade list devices --json says porch-relay is %v", d.DeviceTypes)
		}
	}
	table := string(c.r.stdout(nil, "list", "devices"))
	if !strings.Contains(table, "DEVICE TYPES") || !strings.Contains(table, "OnOffLight") {
		t.Errorf("the table leaves out what it is:\n%s", table)
	}

	c.restartPlugins()
	c.r.until("the light on the bridge again", func() bool { return slices.Contains(c.deviceTypes(endpoint), light) })

	// No longer declared, it's what its driver or its capabilities say.
	c.r.mustDeploy("plugins")
	c.r.until("the relay to be a plug again", func() bool { return slices.Contains(c.deviceTypes(endpoint), plug) })
}

package host

import (
	"encoding/json"
	"net"
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
)

const (
	turnOn  = 0x01
	turnOff = 0x00

	// labelAttribute and uniqueIDAttribute are in Bridged Device Basic Information.
	labelAttribute     = 0x05
	reachableAttribute = 0x11
	uniqueIDAttribute  = 0x12
)

// controller is a Matter controller paired with a rig's host.
type controller struct {
	t     *testing.T
	r     *rig
	port  uint16
	cm    *nodetest.Commissioner
	admin *im.Client
}

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

// matter asks the host about its bridge.
func (r *rig) matter() api.Matter {
	r.t.Helper()
	var m api.Matter
	if err := json.Unmarshal(r.stdout(nil, "matter", "--json"), &m); err != nil {
		r.t.Fatal(err)
	}
	return m
}

func (r *rig) until(what string, cond func() bool) {
	r.t.Helper()
	for deadline := time.Now().Add(waitLimit); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	r.t.Fatalf("timed out waiting for %s. The log:\n%s", what, r.dump())
}

// commissioned starts a host and pairs a controller with it.
func commissioned(t *testing.T) *controller {
	t.Helper()
	port := freeUDP(t)
	r := start(t, matterFlag(port))
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
	c.r.send(id, "switch.on", !want)
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
	c.r.launch(matterFlag(c.port))
	c.r.waitFor(func(l line) bool { return l.is(attached) })

	var after api.Matter
	c.r.until("the fabric", func() bool {
		after = c.r.matter()
		return len(after.Fabrics) == 1
	})
	if after.Window || after.ManualCode != before.ManualCode || after.QRCode != before.QRCode {
		t.Errorf("before %+v, after %+v", before, after)
	}
	c.admin = c.cm.CASE(nodetest.Addr(c.port))
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

	c.r.stdout(nil, "devices", "remove", "hall-lamp")
	c.r.until("the device to leave the bridge", func() bool {
		_, ok := c.read(endpoint, clusters.IDOnOff, 0)
		return !ok
	})
	c.r.stdout(nil, "devices", "reserve", "future-lamp")
	if d := deviceOf(t, c.r.devices(), "future-lamp"); d.Endpoint != 0 {
		if _, ok := c.read(d.Endpoint, clusters.IDOnOff, 0); ok {
			t.Error("a reserved name is on the bridge")
		}
	}
}

func TestARenameKeepsTheBridgedDevice(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	endpoint := c.endpoint("siren")
	unique := c.info(endpoint, uniqueIDAttribute)

	c.r.stdout(nil, "devices", "name", "siren", "alarm", "--allow-safety")
	c.r.until("the new label", func() bool { return c.info(endpoint, labelAttribute) == "alarm" })
	if got := c.info(endpoint, uniqueIDAttribute); got != unique {
		t.Errorf("unique ID %v became %v", unique, got)
	}
	c.reachesHardware("alarm", "siren")

	// A restart builds the endpoint again, which is when a unique ID from the name would change.
	c.r.stop()
	c.r.launch(matterFlag(c.port))
	c.r.waitFor(func(l line) bool { return l.is(attached) })
	c.r.until("the fabric", func() bool { return c.r.matter().Serving })
	c.admin = c.cm.CASE(nodetest.Addr(c.port))
	if got := c.info(endpoint, uniqueIDAttribute); got != unique {
		t.Errorf("after a restart, unique ID %v became %v", unique, got)
	}
}

func TestAControllerGoesThroughArbitration(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	endpoint := c.endpoint("claimed")
	c.command(endpoint, turnOff)
	c.r.until("the Matter claim", func() bool {
		s := onOf(t, c.r.status(), "claimed")
		return s.Value == false && winner(s) == "Matter"
	})
}

func TestASafetyClaimRefusesAController(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	endpoint := c.endpoint("valve")
	c.r.change("leak-sensor", "sensor.contact-open", true)
	c.r.waitFor(func(l line) bool { return l.is(wrote, "device", "valve", "value", false) })
	c.r.until("the controller to see the valve shut", func() bool { return !c.isOn(endpoint) })

	c.command(endpoint, turnOn)
	c.r.waitFor(func(l line) bool {
		return l.is("Failed to set capability from Matter", "capability", "switch.on")
	})
	if !strings.Contains(c.r.dump(), "Rule/leak-shutoff holds it at Safety") {
		t.Errorf("the log doesn't say who refused it:\n%s", c.r.dump())
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
	r.change("leak-sensor", "sensor.contact-open", true)
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
	r.change("leak-sensor", "sensor.contact-open", true)
	r.waitFor(func(l line) bool { return l.is(wrote, "device", "valve", "value", false) })
}

// TestReplacedHardwareReachesTheController is a name taking other hardware, which no driver event says.
func TestReplacedHardwareReachesTheController(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	c.r.pair("old-lamp", "old-lamp")
	c.r.pair("new-lamp", "new-lamp")
	c.r.change("new-lamp", "switch.on", true)
	endpoint := c.endpoint("old-lamp")
	c.r.until("old-lamp on the bridge", func() bool {
		_, ok := c.read(endpoint, clusters.IDOnOff, 0)
		return ok
	})
	if c.isOn(endpoint) {
		t.Fatal("old-lamp is on already")
	}
	c.r.stdout(nil, "devices", "replace", "old-lamp", "new-lamp", "--allow-safety")
	c.r.until("the new hardware's value", func() bool { return c.isOn(endpoint) })
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

	c.r.change("leak-sensor", "sensor.contact-open", true)
	c.r.until("the controller to see the valve shut", func() bool { return !c.isOn(endpoint) })
	c.command(endpoint, turnOn)
	c.r.until("the write to be refused", func() bool { return c.r.count("Failed to set capability from Matter") > 0 })
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

			r.change("leak-sensor", "sensor.contact-open", true)
			r.wrote("valve", false)
			if m := r.matter(); m.Serving || m.Problem == "" || len(m.Fabrics) != 0 {
				t.Errorf("oikade matter --json says %+v", m)
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
	const record = "home/valve"
	r := prepare(t)
	s := r.openStore()
	if err := s.Namespace("controls").Put(record, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	port := freeUDP(t)
	r.launch(matterFlag(port))
	r.waitFor(func(l line) bool { return l.is(attached) })

	stderr, err := r.oikade(nil, "set", "valve", "switch.on", "off")
	if err == nil || !strings.Contains(stderr, "unreadable") {
		t.Errorf("a person's write: %v: %s", err, stderr)
	}
	r.change("leak-sensor", "sensor.contact-open", true)
	r.until("a rule's write to be refused", func() bool {
		return strings.Contains(r.dump(), "unreadable claim record holds valve")
	})
	if n := r.count(wrote, "device", "valve", "value", false); n != 0 {
		t.Errorf("the valve was written %d times", n)
	}
	stderr, err = r.oikade(nil, "devices", "remove", "valve")
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
	r.stdout(nil, "devices", "remove", "valve", "--allow-safety")
	r.stop()
	s = r.openStore()
	defer func() { _ = s.Close() }()
	if raw, ok, _ := s.Namespace("controls").Get(record); ok {
		t.Errorf("the record stayed: %s", raw)
	}
}

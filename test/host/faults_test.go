package host

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/fault"
	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/nodetest"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

// These kill the host at a write that must survive, once it's on disk and before it's answered or
// taken up, and start it again on the same data: what it was told is there, so nothing a caller
// could have heard acknowledged is lost. The journal's are in flow_test.go.

// faultRig is a rig whose host reads its faults from the file plan writes.
func faultRig(t *testing.T) *rig {
	t.Helper()
	r := prepare(t)
	r.env = append(r.env, "OIKADE_FAULTS="+filepath.Join(r.dir, "faults"))
	return r
}

// plan kills the host at point's next hit. A host that's running takes it up from its next hit of
// any point. The plan arrives whole, by a rename, so a host never reads half of one.
func (r *rig) plan(point string) { r.planTo(point, "kill") }

// holdAt stops whichever of the host and your process hits point next there, until a kill.
func (r *rig) holdAt(point string) { r.planTo(point, "wait") }

// unplan leaves the next process or host to run clean: holdAt's plan stays after a kill.
func (r *rig) unplan() {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.dir, "faults")); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) planTo(point, how string) {
	r.t.Helper()
	next := filepath.Join(r.dir, "faults.new")
	if err := os.WriteFile(next, []byte(point+" 1 "+how+"\n"), 0o600); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Rename(next, filepath.Join(r.dir, "faults")); err != nil {
		r.t.Fatal(err)
	}
}

func TestKilledAsTheRegistrySavesKeepsTheChange(t *testing.T) {
	t.Parallel()
	r := faultRig(t)
	r.launchAttached()
	r.pair("shed-lamp", "shed-lamp")
	r.plan(fault.RegistrySaved)
	if _, err := r.oikade(nil, "devices", "remove", "shed-lamp"); err == nil {
		t.Fatalf("the removal was answered. The log:\n%s", r.dump())
	}
	r.died()
	r.launchAttached()
	if out := r.stdout(nil, "list", "devices"); strings.Contains(string(out), "shed-lamp") {
		t.Errorf("the removed device is back after the restart:\n%s", out)
	}
}

// The home's settings are saved before they're applied, so a host that restarts serves the bridge's
// name the running code gave, before any process connects: here, none ever does.
func TestKilledAsTheHomesOptionsSaveKeepsThem(t *testing.T) {
	t.Parallel()
	r := faultRig(t)
	r.plan(fault.HostOptionsSaved)
	r.launchWithout("--home", variant(t, "cottage"), "--simulate", devicesFile(t))
	r.died()

	never, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	port := r.serveMatter(freeUDP(t), func(flag string) {
		hosts := r.count("Started host")
		r.launchWithout("--home", never, "--simulate", devicesFile(t), flag)
		// No process will connect, so its API is up once this host has started.
		r.awaitCount(hosts+1, "Started host")
	})
	r.until("the commissioning window", func() bool { return r.matter().Window })
	c := &controller{t: t, r: r, port: port, cm: nodetest.New(t)}
	c.admin = c.cm.Commission(nodetest.Addr(port), r.matter().Passcode)
	if got := c.nodeLabel(); got != "Cottage" {
		t.Errorf("after the restart, the bridge is named %v", got)
	}
}

func TestKilledAsTheFabricsSaveKeepsThePairing(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	c.r.env = append(c.r.env, "OIKADE_FAULTS="+filepath.Join(c.r.dir, "faults"))
	// The env is the next launch's: start one with it, on the same port and fabric.
	c.r.stop()
	c.relaunch(func(flag string) { c.r.launchAttached(flag) })

	c.r.plan(fault.HostFabricsSaved)
	path := im.CommandPath{Cluster: clusters.IDOperationalCredentials, Command: 0x09} // UpdateFabricLabel
	if res, err := c.admin.Invoke(nodetest.Context(t), path, func(w *tlv.Writer) {
		w.PutUTF8(tlv.Context(0), "Kitchen")
	}, false, 0); err == nil {
		t.Fatalf("the label was answered: %+v. The log:\n%s", res, c.r.dump())
	}
	c.r.died()
	c.relaunch(func(flag string) { c.r.launchAttached(flag) })
	v, ok := c.read(0, clusters.IDOperationalCredentials, 0x0001) // Fabrics
	fabrics, _ := v.([]any)
	if !ok || len(fabrics) != 1 {
		t.Fatalf("fabrics after the restart: %v", v)
	}
	if label := fabrics[0].(map[uint8]any)[5]; label != "Kitchen" {
		t.Errorf("the fabric's label after the restart is %q", label)
	}
}

func TestKilledAsASecretSavesKeepsIt(t *testing.T) {
	t.Parallel()
	r := faultRig(t)
	r.launchAttached()
	r.plan(fault.HostSecretSaved)
	if err := r.setSecret("api-key"); err == nil {
		t.Fatalf("the secret was answered. The log:\n%s", r.dump())
	}
	r.died()
	r.launchAttached()
	if out := r.stdout(nil, "list", "secrets"); !strings.Contains(string(out), "api-key") {
		t.Errorf("the secret is gone after the restart:\n%s", out)
	}
	if got, err := os.ReadFile(filepath.Join(r.dir, "secrets", "api-key")); err != nil || string(got) != secretValue {
		t.Errorf("the secret's value after the restart is %q, %v", got, err)
	}
}

// A deploy killed as its upload moves among the versions is no deploy: the old version runs after a
// restart, nothing is left among the uploads, and the same deploy then works.
func TestKilledAsAnUploadMovesDeploysNothingAndLeavesNothing(t *testing.T) {
	t.Parallel()
	for _, point := range []string{fault.HostUploadBefore, fault.HostUploadAfter} {
		t.Run(point, func(t *testing.T) {
			t.Parallel()
			r := faultRig(t)
			r.launchAttached()
			r.sync()
			r.plan(point)
			if stderr, err := r.deploy("second"); err == nil {
				t.Fatalf("the deploy was answered: %s. The log:\n%s", stderr, r.dump())
			}
			r.died()
			r.launchAttached()
			r.sync()
			if left, err := os.ReadDir(filepath.Join(r.dir, "uploads")); err != nil || len(left) != 0 {
				t.Errorf("left %v among the uploads: %v", left, err)
			}
			// Nothing was deployed before, so no binary is kept: not even the one the kill left.
			if kept, err := os.ReadDir(filepath.Join(r.dir, "versions")); err != nil || len(kept) != 0 {
				t.Errorf("kept %v among the versions: %v", kept, err)
			}
			if n := r.count(deployed); n != 0 {
				t.Errorf("%d deploys finished. The log:\n%s", n, r.dump())
			}
			r.mustDeploy("second")
		})
	}
}

// OIK-299: a case is saved as started before it claims, so your process killed right after the claim
// keeps the case's latch. Here the overheat passes while the process is held there, and the heater
// stays off until the door opens, rather than held off by a claim nothing ends.
func TestACaseKilledAsItClaimsKeepsItsLatch(t *testing.T) {
	t.Parallel()
	r := faultRig(t)
	r.launch()
	r.waitFor(func(l line) bool { return l.is(attached) })
	r.sync()
	r.holdAt(fault.ClientHoldStarted)
	r.change("garage-sensor", "temperature", 45)
	r.wrote("garage-heater", false)
	r.change("garage-sensor", "temperature", 20)
	r.unplan()
	r.kill()
	r.sync()
	// The latch outlived the kill: a hold that forgot it would turn the heater back on here.
	if n := r.count(wrote, "device", "garage-heater", "value", true); n != 0 {
		t.Fatalf("the heater came back on before the door opened\n%s", r.dump())
	}
	r.change("garage-door", "open", true)
	r.until("the heater back on", func() bool { return onOf(t, r.status("garage-heater"), "garage-heater").Value == true })
}

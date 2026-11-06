package registry

import (
	"bytes"
	"errors"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/store"
)

func open(t *testing.T) (*Registry, *store.Namespace) {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	values := s.Namespace(Namespace)
	r, err := Open(values)
	if err != nil {
		t.Fatal(err)
	}
	return r, values
}

func report(t *testing.T, r *Registry, id, name string) Device {
	t.Helper()
	d, err := r.Report(Report{ID: id, Driver: "sim", Name: name, Capabilities: []string{"switch.on"}})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func get(t *testing.T, r *Registry, name string) Device {
	t.Helper()
	d, ok := r.Get(name)
	if !ok {
		t.Fatalf("no device %s in %+v", name, r.List())
	}
	return d
}

func TestAPairedDeviceEntersOnceAndKeepsItsName(t *testing.T) {
	r, _ := open(t)
	d, err := r.Report(Report{ID: "0x1", Driver: "sim", Name: "Hall Lamp!", Room: "Hall", Capabilities: []string{"switch.on"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "hall-lamp" || d.Room != "Hall" || d.Endpoint != FirstEndpoint {
		t.Errorf("got %+v", d)
	}
	if err := r.Rename("hall-lamp", "lamp"); err != nil {
		t.Fatal(err)
	}
	if err := r.Move("lamp", "Attic"); err != nil {
		t.Fatal(err)
	}
	again, err := r.Report(Report{ID: "0x1", Driver: "sim", Name: "Renamed In The App", Room: "Elsewhere", Capabilities: []string{"switch.on", "light.level"}})
	if err != nil {
		t.Fatal(err)
	}
	if again.Name != "lamp" || again.Room != "Attic" || again.Endpoint != d.Endpoint || len(again.Capabilities) != 2 {
		t.Errorf("an app's names were followed, or capabilities weren't: %+v", again)
	}
}

func TestNamesFromAppsAreUniqueAndValid(t *testing.T) {
	r, _ := open(t)
	if a, b := report(t, r, "1", "Lamp"), report(t, r, "2", "lamp"); a.Name != "lamp" || b.Name != "lamp-2" {
		t.Errorf("got %s and %s", a.Name, b.Name)
	}
	for name, id := range map[string]string{"": "0x00124B", "2nd floor": "x", "---": "", "Ünï": "y"} {
		if err := checkName(Slug(name, id)); err != nil {
			t.Errorf("Slug(%q, %q) = %q: %v", name, id, Slug(name, id), err)
		}
	}
	if err := checkName(Slug(strings.Repeat("ab-", 40), "x")); err != nil {
		t.Error(err)
	}
}

func TestEndpointsAreNeverReused(t *testing.T) {
	r, _ := open(t)
	lamp, fan := report(t, r, "lamp", "lamp"), report(t, r, "fan", "fan")
	if err := r.Remove("lamp"); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove("fan"); err != nil {
		t.Fatal(err)
	}
	if err := r.Reserve("lamp", ""); err != nil {
		t.Fatal(err)
	}
	if got := get(t, r, "lamp").Endpoint; got == lamp.Endpoint || got == fan.Endpoint {
		t.Errorf("a new device took endpoint %d", got)
	}
	other := report(t, r, "other", "other")
	if other.Endpoint == lamp.Endpoint || other.Endpoint == fan.Endpoint {
		t.Errorf("new hardware took endpoint %d", other.Endpoint)
	}

	back := report(t, r, "fan", "ignored")
	if back.Endpoint != fan.Endpoint || back.Name != "fan" {
		t.Errorf("removed hardware came back as %+v, want %+v", back, fan)
	}
	again := report(t, r, "lamp", "ignored")
	if again.Endpoint != lamp.Endpoint || again.Name != "lamp-2" {
		t.Errorf("removed hardware whose name went came back as %+v", again)
	}
	seen := map[uint16]bool{}
	list := r.List()
	for _, d := range list.Devices {
		if seen[d.Endpoint] || d.Endpoint >= list.Next {
			t.Errorf("endpoint %d reused or past next in %+v", d.Endpoint, list)
		}
		seen[d.Endpoint] = true
	}
}

func TestTheRegistrySurvivesReopening(t *testing.T) {
	r, values := open(t)
	report(t, r, "lamp", "lamp")
	if err := r.Remove("lamp"); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(values)
	if err != nil {
		t.Fatal(err)
	}
	if d := report(t, reopened, "fan", "fan"); d.Endpoint != FirstEndpoint+1 {
		t.Errorf("the counter was lost: %+v", d)
	}
	if d := report(t, reopened, "lamp", "lamp"); d.Endpoint != FirstEndpoint {
		t.Errorf("the removed device was lost: %+v", d)
	}
}

func TestAnUnreadableRegistryIsAnError(t *testing.T) {
	_, values := open(t)
	for _, raw := range []string{"not json", `{"next":0,"devices":[]}`} {
		if err := values.Put(stateKey, []byte(raw)); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(values); err == nil {
			t.Errorf("%s opened as a registry", raw)
		}
	}
}

func TestAFailedSaveChangesNothing(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(s.Namespace(Namespace))
	if err != nil {
		t.Fatal(err)
	}
	report(t, r, "lamp", "lamp")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Reserve("fan", ""); err == nil {
		t.Fatal("saved to a closed store")
	}
	if list := r.List(); list.Next != FirstEndpoint+1 || len(list.Devices) != 1 {
		t.Errorf("took up a change it couldn't save: %+v", list)
	}
}

func TestReplacingKeepsTheNameAndEndpoint(t *testing.T) {
	r, _ := open(t)
	old := report(t, r, "old", "lamp")
	fresh := report(t, r, "new", "new-lamp")
	if err := r.Replace("lamp", "new-lamp"); err != nil {
		t.Fatal(err)
	}
	d := get(t, r, "lamp")
	if d.ID != "new" || d.Endpoint != old.Endpoint {
		t.Errorf("got %+v", d)
	}
	if _, ok := r.Get("new-lamp"); ok {
		t.Error("the replacement's name stayed")
	}
	if byID, ok := r.ByID("sim", "new"); !ok || byID.Name != "lamp" {
		t.Errorf("its hardware is %+v, %v", byID, ok)
	}
	if again := report(t, r, "new", "ignored"); again.Name != "lamp" || again.Endpoint != old.Endpoint {
		t.Errorf("the next report moved it: %+v", again)
	}
	back := report(t, r, "old", "old-lamp")
	if back.Endpoint == old.Endpoint || back.Endpoint == fresh.Endpoint {
		t.Errorf("the old hardware took endpoint %d", back.Endpoint)
	}

	if err := r.Reserve("porch", "Porch"); err != nil {
		t.Fatal(err)
	}
	reserved := get(t, r, "porch")
	if !reserved.Reserved() {
		t.Errorf("not reserved: %+v", reserved)
	}
	report(t, r, "paired", "paired")
	if err := r.Replace("porch", "paired"); err != nil {
		t.Fatal(err)
	}
	if d := get(t, r, "porch"); d.ID != "paired" || d.Endpoint != reserved.Endpoint || d.Room != "Porch" || d.Reserved() {
		t.Errorf("a reserved name's hardware: %+v", d)
	}
	if err := r.Replace("porch", "porch"); err == nil {
		t.Error("replaced a device with itself")
	}
	if err := r.Reserve("empty", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.Replace("lamp", "empty"); err == nil {
		t.Error("moved onto no hardware")
	}
	if err := r.Replace("nothing", "lamp"); !errors.Is(err, ErrNoDevice) {
		t.Errorf("got %v", err)
	}
}

func TestRestoreRebuildsARegistryThatWasLost(t *testing.T) {
	r, _ := open(t)
	report(t, r, "a", "first")
	second := report(t, r, "b", "second")
	report(t, r, "c", "gone")
	if err := r.Remove("gone"); err != nil {
		t.Fatal(err)
	}
	if err := r.Move("second", "Hall"); err != nil {
		t.Fatal(err)
	}
	if err := r.Reserve("porch", "Porch"); err != nil {
		t.Fatal(err)
	}
	file, err := Render(r.List(), "main")
	if err != nil {
		t.Fatal(err)
	}
	copied, err := Parse(file)
	if err != nil {
		t.Fatal(err)
	}

	lost, _ := open(t)
	// The drivers got there first, in another order, under the names from their apps.
	if d := report(t, lost, "b", "from-the-app"); d.Endpoint != FirstEndpoint {
		t.Fatalf("got %+v", d)
	}
	report(t, lost, "new", "not-in-the-file")
	moved, err := lost.Restore(copied)
	if err != nil {
		t.Fatal(err)
	}
	if d := get(t, lost, "second"); d.ID != "b" || d.Endpoint != second.Endpoint || d.Room != "Hall" {
		t.Errorf("got %+v", d)
	}
	if d := get(t, lost, "porch"); !d.Reserved() {
		t.Errorf("got %+v", d)
	}
	if _, ok := lost.Get("gone"); ok {
		t.Error("a removed device came back")
	}
	if back := report(t, lost, "c", "ignored"); back.Name != "gone" || back.Endpoint != FirstEndpoint+2 {
		t.Errorf("a removed device lost its endpoint: %+v", back)
	}
	kept := get(t, lost, "not-in-the-file")
	if len(moved) != 1 || moved[0] != kept.Name || kept.Endpoint < copied.Next {
		t.Errorf("moved %v, kept %+v, next %d", moved, kept, copied.Next)
	}
	list := lost.List()
	if d := report(t, lost, "later", "later"); d.Endpoint < list.Next {
		t.Errorf("handed out %d below next %d", d.Endpoint, list.Next)
	}
	seen := map[uint16]bool{}
	for _, d := range lost.List().Devices {
		if seen[d.Endpoint] {
			t.Errorf("endpoint %d used twice", d.Endpoint)
		}
		seen[d.Endpoint] = true
	}
}

func TestRestoringAnOldCopyNeverLowersNext(t *testing.T) {
	r, _ := open(t)
	report(t, r, "a", "first")
	old := r.List()
	report(t, r, "b", "second")
	after := r.List().Next
	if _, err := r.Restore(old); err != nil {
		t.Fatal(err)
	}
	if got := r.List().Next; got != after {
		t.Errorf("next went from %d to %d", after, got)
	}
	if d := get(t, r, "second"); d.Endpoint != FirstEndpoint+1 {
		t.Errorf("got %+v", d)
	}
}

func TestRestoreRefusesABadCopy(t *testing.T) {
	r, _ := open(t)
	report(t, r, "a", "first")
	for name, s := range map[string]Snapshot{
		"no next":       {},
		"past next":     {Next: 3, Devices: []Device{{Name: "a", Endpoint: 3}}},
		"root endpoint": {Next: 3, Devices: []Device{{Name: "a", Endpoint: 1}}},
		"same endpoint": {Next: 5, Devices: []Device{{Name: "a", Endpoint: 2}, {Name: "b", Endpoint: 2}}},
		"same name":     {Next: 5, Devices: []Device{{Name: "a", Endpoint: 2}, {Name: "a", Endpoint: 3}}},
		"same hardware": {Next: 5, Devices: []Device{{Name: "a", ID: "x", Endpoint: 2}, {Name: "b", ID: "x", Endpoint: 3}}},
		"a bad name":    {Next: 3, Devices: []Device{{Name: "Not Valid", Endpoint: 2}}},
	} {
		before := r.List()
		if _, err := r.Restore(s); err == nil {
			t.Errorf("%s: restored", name)
		}
		if after := r.List(); after.Next != before.Next || len(after.Devices) != len(before.Devices) {
			t.Errorf("%s: changed the registry", name)
		}
	}
}

func TestRenderIsDeterministicAndValidGo(t *testing.T) {
	r, _ := open(t)
	report(t, r, "z", "zebra-lamp")
	report(t, r, "a", "apple-lamp")
	if err := r.Reserve("porch", "Front\nconst Evil = 1\n"); err != nil {
		t.Fatal(err)
	}
	first, err := Render(r.List(), "home")
	if err != nil {
		t.Fatal(err)
	}
	shuffled := r.List()
	slices.Reverse(shuffled.Devices)
	second, err := Render(shuffled, "home")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("two renders differ:\n%s\n%s", first, second)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "devices.go", first, parser.ParseComments)
	if err != nil {
		t.Fatalf("%v\n%s", err, first)
	}
	if f.Name.Name != "home" {
		t.Errorf("package %s", f.Name.Name)
	}
	if strings.Contains(string(first), "\nconst Evil") {
		t.Errorf("a room wrote code:\n%s", first)
	}
	for _, want := range []string{`AppleLamp = "apple-lamp"`, `ZebraLamp = "zebra-lamp"`, `Porch = "porch"`, "reserved"} {
		if !strings.Contains(string(first), want) {
			t.Errorf("no %q in:\n%s", want, first)
		}
	}
}

func TestRenderLeavesOutRemovedDevices(t *testing.T) {
	r, _ := open(t)
	report(t, r, "a", "lamp")
	if err := r.Remove("lamp"); err != nil {
		t.Fatal(err)
	}
	file, err := Render(r.List(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(file), "Lamp =") {
		t.Errorf("the constant stayed:\n%s", file)
	}
	back, err := Parse(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Devices) != 1 || !back.Devices[0].Removed || back.Next != FirstEndpoint+1 {
		t.Errorf("got %+v", back)
	}
}

func TestRenderRefusesNamesThatMeetInGo(t *testing.T) {
	r, _ := open(t)
	for _, name := range []string{"a-1", "a1"} {
		if err := r.Reserve(name, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Render(r.List(), "main"); err == nil || !strings.Contains(err.Error(), "A1") {
		t.Errorf("got %v", err)
	}
}

func TestParseRefusesOtherFiles(t *testing.T) {
	if _, err := Parse([]byte("package main\n")); err == nil {
		t.Error("read a file oikade types didn't write")
	}
	if _, err := Parse([]byte(registryLine + "{\n")); err == nil {
		t.Error("read a broken file")
	}
}

func TestChecksOnNames(t *testing.T) {
	r, _ := open(t)
	report(t, r, "a", "lamp")
	for _, name := range []string{"", "Lamp", "9lamp", "la mp", "lamp-", "la--mp", strings.Repeat("a", 64)} {
		if err := r.Reserve(name, ""); err == nil {
			t.Errorf("reserved %q", name)
		}
	}
	if err := r.Reserve("lamp", ""); err == nil {
		t.Error("reserved a name in use")
	}
	if err := r.Reserve("fan", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.Rename("lamp", "fan"); err == nil {
		t.Error("renamed onto a name in use")
	}
	if err := r.Rename("nothing", "x"); !errors.Is(err, ErrNoDevice) {
		t.Errorf("got %v", err)
	}
}

func TestEndpointsRunOut(t *testing.T) {
	s := Snapshot{Next: LastEndpoint}
	if got, err := allocate(&s); err != nil || got != LastEndpoint {
		t.Fatalf("got %d, %v", got, err)
	}
	if _, err := allocate(&s); err == nil {
		t.Error("handed out an endpoint Matter doesn't have")
	}
}

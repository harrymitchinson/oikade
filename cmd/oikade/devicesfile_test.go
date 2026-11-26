package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/registry"
	"oikade.dev/internal/testkit"
)

func TestTypesAddsALineForEachNewDevice(t *testing.T) {
	plafond := newDevice{name: "Cuisine - Plafond", key: "hue/0x2", room: "cuisine", typ: devicetype.DimmableLight}
	capteur := newDevice{
		name: "Cuisine détecteur", key: "hue/0x1", room: "cuisine", typ: devicetype.OccupancySensor,
		more: []string{"battery", "temperature"},
	}
	prise := newDevice{name: "1002a9b9e2", key: "ewelink/1002a9b9e2", typ: devicetype.OnOffPlugInUnit}
	salon := newDevice{name: "Salon", key: "toshibaac/ac1", room: "salon", typ: devicetype.RoomAirConditioner}

	got, added, err := addDevices(nil, "main", nil, []newDevice{plafond, capteur, prise, salon})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := format.Source(got); err != nil {
		t.Errorf("it isn't gofmt's: %v", err)
	}
	testkit.Golden(t, "types-new-file", got)
	// A device named like its room ends with a hash of its hardware.
	if added.names["toshibaac/ac1"] != "Salon"+hash8("hardware/toshibaac/ac1") || len(added.clashed) != 1 {
		t.Errorf("added %+v", added)
	}

	// What's there stays byte for byte, hand edits included, and only what's new is added, in its
	// own rooms where the code has them.
	edited := strings.Replace(string(got), "CuisinePlafond   = ", "Plafond = ", 1)
	edited = strings.Replace(edited, `, Cuisine)`+"\n\tDevice1002a9b9e2", `, Cuisine) // moved`+"\n\tPrise", 1)
	edited += "\n// Not paired yet.\n"
	entree := newDevice{name: "Entrée - Détecteur", key: "hue/0x3", room: "entree", typ: devicetype.OccupancySensor}
	lampe := newDevice{name: "Lampe", key: "hue/0x4", room: "cuisine", typ: devicetype.OnOffLight}
	again, added, err := addDevices([]byte(edited), "main", nil, []newDevice{entree, lampe})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(again, []byte(edited)) {
		t.Errorf("what was there changed:\n%s", again)
	}
	testkit.Golden(t, "types-added", again)
	if len(added.rooms) != 1 || added.rooms[0] != "Entree" {
		t.Errorf("added the rooms %v", added.rooms)
	}

	// Nothing new adds nothing, and hardware the file names already is nothing new, whatever the
	// home's build says.
	same, _, err := addDevices(again, "main", nil, []newDevice{plafond, entree})
	if err != nil || !bytes.Equal(same, again) {
		t.Errorf("adding nothing gave %v:\n%s", err, same)
	}
}

func TestTypesWritesNoCodeFromWhatAnAppSays(t *testing.T) {
	evil := "salon\")\nconst Evil = 1 //"
	got, _, err := addDevices(nil, "main", nil, []newDevice{
		{name: "Lampe\nconst Evil = 2", key: "hue/0x1\")\nconst Evil = 3 //", room: evil, typ: devicetype.OnOffLight},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "\nconst Evil") {
		t.Errorf("wrote code:\n%s", got)
	}
}

func TestTypesAddsTheImportAndKeepsNamesTheRestOfThePackageHas(t *testing.T) {
	file := "package home\n\n// Mine.\nvar x = 1\n"
	others := [][]byte{
		[]byte("package home\n\nimport o \"oikade.dev\"\n\nvar Bureau = o.Room(\"bureau\")\n\nfunc Lampe() {}\n"),
		[]byte("package elsewhere\n\nfunc Prise() {}\n"),
	}
	lampe := newDevice{name: "lampe", key: "hue/0x4", room: "bureau", typ: devicetype.OnOffLight}
	prise := newDevice{name: "prise", key: "hue/0x5", typ: devicetype.OnOffPlugInUnit}
	got, added, err := addDevices([]byte(file), "main", others, []newDevice{lampe, prise})
	if err != nil {
		t.Fatal(err)
	}
	testkit.Golden(t, "types-import", got)
	if added.names["hue/0x5"] != "Prise" || !strings.HasPrefix(added.names["hue/0x4"], "Lampe") ||
		added.names["hue/0x4"] == "Lampe" || len(added.rooms) != 0 {
		t.Errorf("added %+v", added)
	}
}

func TestTypesRefusesAFileThatIsntGo(t *testing.T) {
	if _, _, err := addDevices([]byte("package main\nvar ("), "main", nil, nil); err == nil {
		t.Error("it added to a file that isn't Go")
	}
}

func TestTypesNamesEachDeviceFromItsApp(t *testing.T) {
	list := registry.Snapshot{Devices: []registry.Device{
		{AppName: "Salon - Lampe d'appoint", ID: "0x1", Driver: "plugin/hue", Room: "salon",
			Capabilities: []string{"on", "brightness", "link-quality", "from-a-newer-plugin"}},
		{ID: "1002a9b9e2", Driver: "plugin/ewelink", Capabilities: []string{"on"}},
		{ID: "0x9", Driver: "plugin/hue"},
		{ID: "0x8", Driver: "plugin/hue", Removed: true, Capabilities: []string{"on"}},
	}}
	devices, unknown := toAdd(list)
	want := []newDevice{
		{
			name: "Salon - Lampe d'appoint", key: "hue/0x1", room: "salon", typ: devicetype.DimmableLight,
			more: []string{"link-quality"},
		},
		{name: "1002a9b9e2", key: "ewelink/1002a9b9e2", typ: devicetype.OnOffPlugInUnit},
	}
	if len(devices) != len(want) || len(unknown) != 1 || unknown[0].Key() != "hue/0x9" {
		t.Fatalf("got %+v, and %v", devices, unknown)
	}
	for i := range want {
		if g, w := devices[i], want[i]; g.name != w.name || g.key != w.key || g.room != w.room || g.typ != w.typ ||
			strings.Join(g.more, ",") != strings.Join(w.more, ",") {
			t.Errorf("got %+v, want %+v", g, w)
		}
	}
}

func TestTypesReadsThePackagesOtherFiles(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"devices.go": "package main\n", "main.go": "package main\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	others, err := packageFiles(filepath.Join(dir, "devices.go"))
	if err != nil || len(others) != 1 {
		t.Errorf("got %d files, %v", len(others), err)
	}
}

func hash8(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(sum[:])[:8]
}

func TestTypesSaysWhatItAddedAndWhatToDoNext(t *testing.T) {
	got := added{
		names:   map[string]string{"ewelink/1002a9b9e2": "PriseGarage", "toshibaac/ac1": "Salon96196d18"},
		rooms:   []string{"Salon"},
		clashed: []string{"Salon96196d18"},
	}
	var b bytes.Buffer
	unknown := []registry.Device{
		{ID: "0x9", Driver: "plugin/hue"}, {ID: "0x7", Driver: "plugin/hue", Capabilities: []string{"battery"}},
	}
	printAdded(printerOf(&b), "devices.go", true, true, got, unknown)
	for _, want := range []string{
		"Would replace devices.go, which an older oikade types wrote",
		"Would add 2 devices and 1 room to devices.go: PriseGarage and Salon96196d18",
		"Salon96196d18 had a name that was taken",
		"Left out hue/0x9, which hasn't reported yet",
		"Left out hue/0x7, as no device type fits",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %q in:\n%s", want, b.String())
		}
	}
	b.Reset()
	printAdded(printerOf(&b), "devices.go", false, false, added{}, nil)
	if !strings.Contains(b.String(), "devices.go has every device on record") {
		t.Errorf("said %q", b.String())
	}
	b.Reset()
	printAdded(printerOf(&b), "devices.go", false, false, added{}, unknown)
	if strings.Contains(b.String(), "every device") || !strings.Contains(b.String(), "Nothing to add to devices.go") {
		t.Errorf("with devices left out, said %q", b.String())
	}
}

func TestTypesKeepsAnOldFilesPackageAndImportsWhatItNeeds(t *testing.T) {
	old := []byte("// Code generated by oikade types. DO NOT EDIT.\n\npackage home\n")
	if got := packageOf(old, "main"); got != "home" {
		t.Errorf("an old file's package is %q", got)
	}
	blank := "package main\n\nimport _ \"oikade.dev\"\n"
	lampe := newDevice{name: "lampe", key: "hue/0x1", typ: devicetype.OnOffLight}
	got, _, err := addDevices([]byte(blank), "main", nil, []newDevice{lampe})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := format.Source(got); err != nil || !strings.Contains(string(got), "\n\nimport \"oikade.dev\"") {
		t.Errorf("got %v:\n%s", err, got)
	}
}

func TestTypesWritesItsFileWholeOrNotAtAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.go")
	if err := writeAtomically(path, []byte("package main\n")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if raw, _ := os.ReadFile(path); string(raw) != "package main\n" || len(entries) != 1 {
		t.Errorf("wrote %q, beside %d files", raw, len(entries)-1)
	}
}

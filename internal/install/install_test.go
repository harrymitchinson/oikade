package install

import (
	"strings"
	"testing"
)

func TestUnitRunsTheSameCommandAsAnyHost(t *testing.T) {
	unit, err := Unit("pi", "/var/lib/oikade", []string{"--matter-name", `Our "home" 100%`, "--broker=mqtt://$x"})
	if err != nil {
		t.Fatal(err)
	}
	want := `ExecStart=/usr/local/bin/oikade host --data-dir /var/lib/oikade --matter-name "Our \"home\" 100%%" --broker=mqtt://$$x` + "\n"
	if !strings.Contains(unit, want) {
		t.Errorf("no %s in:\n%s", want, unit)
	}
	for _, line := range []string{"User=pi\n", "Restart=always\n", "WantedBy=multi-user.target\n"} {
		if !strings.Contains(unit, line) {
			t.Errorf("no %s in:\n%s", line, unit)
		}
	}
}

func TestUnitIsTheSameEachTime(t *testing.T) {
	args := []string{"--broker", "mqtt://broker:1883"}
	a, err := Unit("pi", "/data", args)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Unit("pi", "/data", args)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("a second render differs, so a second install would restart the host")
	}
}

func TestUnitRefusesALineBreak(t *testing.T) {
	if _, err := Unit("pi", "/data", []string{"a\n[Service]"}); err == nil {
		t.Error("a flag added a line to the unit")
	}
}

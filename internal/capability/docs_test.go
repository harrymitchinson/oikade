package capability

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/testkit"
)

const docsTable = "../../docs/capabilities.md"

// table is docs/capabilities.md, from the registry.
func table() string {
	var b strings.Builder
	b.WriteString("<!-- Generated from internal/capability by its tests: go test ./internal/capability -update -->\n\n")
	b.WriteString("# Capabilities\n\n")
	b.WriteString("Every capability Oikade knows, named as a person would say them rather than as a protocol does. ")
	b.WriteString("A home names them through `oikade`'s constants and the handles `oikade types` writes, and a ")
	b.WriteString("plugin through `contract/v1`'s. Any other name is unknown. ")
	b.WriteString("[Devices](devices.md#capabilities) says what each one means, and how the Matter bridge shows it.\n\n")
	b.WriteString("A `name` capability's value is one of its names. The number in brackets after each is what claims ")
	b.WriteString("hold and the `oikade_capability_value` gauge exports, and names are only ever added, so a number ")
	b.WriteString("keeps its name.\n\n")
	b.WriteString("| Name | In a home | Type | Range | Writable | Matter |\n|---|---|---|---|---|---|\n")
	for _, name := range Names() {
		s := registry[name]
		kind, span := "bool", ""
		switch s.Kind {
		case KindNumber:
			kind = "number"
			span = fmt.Sprintf("%g to %g%s", s.Min, s.Max, s.Unit)
		case KindName:
			kind = "name"
			var names []string
			for i, n := range s.Names {
				names = append(names, fmt.Sprintf("`%s` (%d)", n, i))
			}
			span = strings.Join(names, ", ")
		}
		writable := ""
		if s.Writable {
			writable = "yes"
		}
		matter := "none"
		if m := s.Matter; m != nil {
			matter = fmt.Sprintf("%s (0x%04X) %s (0x%04X)", m.Cluster, m.ClusterID, m.Attribute, m.AttributeID)
			if m.Inverted {
				matter += ", inverted"
			}
		}
		fmt.Fprintf(&b, "| `%s` | `oikade.%s` | %s | %s | %s | %s |\n", name, s.Go, kind, span, writable, matter)
	}
	return b.String()
}

func TestTheDocsTableIsTheRegistrys(t *testing.T) {
	want := table()
	if *testkit.Update {
		if err := os.WriteFile(docsTable, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(docsTable)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s is out of date: go test ./internal/capability -update", docsTable)
	}
}

// The Matter bridge shows each capability as the attribute the registry names, so every one has one
// but link-quality, which Matter has no attribute for. Some need another with them: a mode shows on
// an air conditioner, with a setpoint and an on, and a brightness on a light.
func TestEveryCapabilityButLinkQualityHasAMatterAttribute(t *testing.T) {
	for _, name := range Names() {
		m := registry[name].Matter
		switch {
		case name == LinkQuality && m != nil:
			t.Errorf("%s has a Matter attribute", name)
		case name != LinkQuality && (m == nil || m.Cluster == "" || m.Attribute == "" || m.ClusterID == 0):
			t.Errorf("%s has no Matter cluster and attribute: %+v", name, m)
		}
	}
	if !registry[Open].Matter.Inverted {
		t.Error("open is true while open, and Boolean State true while shut")
	}
}

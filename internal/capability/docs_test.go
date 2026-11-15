package capability

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite docs/capabilities.md from the registry")

const docsTable = "../../docs/capabilities.md"

// table is docs/capabilities.md, from the registry.
func table() string {
	var b strings.Builder
	b.WriteString("<!-- Generated from internal/capability by its tests: go test ./internal/capability -update -->\n\n")
	b.WriteString("# Capabilities\n\n")
	b.WriteString("Every capability Oikade knows, named for people rather than for a protocol ")
	b.WriteString("([OIK-181](https://linear.app/oikade/issue/OIK-181)). A home names them through `framework`'s ")
	b.WriteString("constants and the handles `oikade types` writes, and a plugin through `contract/v1`'s. ")
	b.WriteString("Any other name is unknown.\n\n")
	b.WriteString("| Name | In a home | Type | Range | Writable | Matter |\n|---|---|---|---|---|---|\n")
	for _, name := range Names() {
		s := registry[name]
		kind, span := "bool", ""
		if s.Kind == KindNumber {
			kind = "number"
			span = fmt.Sprintf("%g to %g%s", s.Min, s.Max, s.Unit)
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
		fmt.Fprintf(&b, "| `%s` | `framework.%s` | %s | %s | %s | %s |\n", name, s.Go, kind, span, writable, matter)
	}
	return b.String()
}

func TestTheDocsTableIsTheRegistrys(t *testing.T) {
	want := table()
	if *update {
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
// but link-quality, which Matter has no attribute for.
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

package control

import (
	"strings"
	"testing"
)

func TestARulesRefAndKeyGoBothWays(t *testing.T) {
	for key, ref := range map[string]string{
		"desk-plug on night": "case/desk-plug/on/night",
		"morning":            "flow/morning",
	} {
		if got := RuleRef(key).String(); got != ref {
			t.Errorf("RuleRef(%q) = %s, want %s", key, got, ref)
		}
		if got, err := RuleKey(ref); err != nil || got != key {
			t.Errorf("RuleKey(%s) = %q, %v, want %q", ref, got, err, key)
		}
	}
	for _, ref := range []string{"device/desk-plug", "morning", "case/desk-plug/on"} {
		if _, err := RuleKey(ref); err == nil || !strings.Contains(err.Error(), "a rule is a case") {
			t.Errorf("RuleKey(%s) = %v", ref, err)
		}
	}
}

func TestSameRuleIsARefThatReadsBackToTheKey(t *testing.T) {
	if !SameRule("case/desk-plug/on/night", "desk-plug on night") || SameRule("flow/Good%20Morning", "Good Morning") {
		t.Error("SameRule took a ref that doesn't read back")
	}
}

func TestAClaimsSourceIsARef(t *testing.T) {
	for source, ref := range map[string]string{
		"Rule/desk-plug on night": "case/desk-plug/on/night",
		"Rule/morning":            "flow/morning",
		SourceMatter:              "matter",
		SourceDevice:              "device",
		SourceCommand:             "command",
	} {
		if got := SourceRef(source).String(); got != ref {
			t.Errorf("SourceRef(%q) = %s, want %s", source, got, ref)
		}
	}
}

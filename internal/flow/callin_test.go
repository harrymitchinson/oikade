package flow

import (
	"encoding/json"
	"testing"
)

func TestCallInReadsAnOutsideCall(t *testing.T) {
	raw, err := json.Marshal(entry{Kind: kindDo, ID: "plan", Result: json.RawMessage(`"long"`)})
	if err != nil {
		t.Fatal(err)
	}
	name, result, failure, ok := CallIn(raw)
	if !ok || name != "plan" || result != "long" || failure != "" {
		t.Fatalf("CallIn = %q, %v, %q, %v", name, result, failure, ok)
	}
	failed, err := json.Marshal(entry{Kind: kindDo, ID: "plan", Err: "no"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, failure, ok := CallIn(failed); !ok || failure != "no" {
		t.Fatalf("CallIn = %q, %v", failure, ok)
	}
	other, err := json.Marshal(entry{Kind: kindNow})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := CallIn(other); ok {
		t.Fatal("CallIn took another step for a call")
	}
}

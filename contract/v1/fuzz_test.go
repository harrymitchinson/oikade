package contract

import (
	"reflect"
	"testing"
)

// FuzzParseSet feeds ParseSet a /set message, as anything on the broker can publish one to a
// plugin. A command it accepts encodes, by Payload, to one it reads back the same.
func FuzzParseSet(f *testing.F) {
	r := NewRoot("ewelink")
	f.Add("1000aabbcc", []byte(`{"on":true}`))
	f.Add("1000aabbcc", []byte(`{"brightness":40.5}`))
	f.Add("1000aabbcc", []byte(`{"on":true,"on":false}`))
	f.Add("a/b", []byte(`{"on":1e400}`))
	f.Add("", []byte(`{"":0}`))
	f.Fuzz(func(t *testing.T, id string, payload []byte) {
		cmd, ok, err := r.ParseSet(r.Set(id), payload)
		if !ok || err != nil {
			return
		}
		if cmd.ID != id {
			t.Fatalf("ParseSet read ID %q from %q", cmd.ID, r.Set(id))
		}
		again, err := cmd.Payload()
		if err != nil {
			t.Fatalf("Payload(%+v): %v", cmd, err)
		}
		got, ok, err := r.ParseSet(r.Set(cmd.ID), again)
		if !ok || err != nil || !reflect.DeepEqual(got, cmd) {
			t.Fatalf("%s read as %+v, and its Payload %s as %+v, %v, %v", payload, cmd, again, got, ok, err)
		}
	})
}

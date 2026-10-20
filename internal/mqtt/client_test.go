package mqtt

import (
	"context"
	"testing"
)

func TestMatch(t *testing.T) {
	const all, lamp = "z2m/#", "z2m/lamp"
	for _, tc := range []struct {
		filter, topic string
		want          bool
	}{
		{all, lamp, true},
		{all, "z2m/lamp/availability", true},
		{all, "other/lamp", false},
		{"z2m/+", lamp, true},
		{"z2m/+", "z2m/lamp/set", false},
		{"z2m/+/set", "z2m/lamp/set", true},
		{lamp, lamp, true},
		{lamp, "z2m", false},
		{"#", "anything/at/all", true},
	} {
		if got := Match(tc.filter, tc.topic); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.filter, tc.topic, got, tc.want)
		}
	}
}

func TestMemoryRetained(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	_ = m.Publish(ctx, "a/retained", []byte("1"), true)
	_ = m.Publish(ctx, "a/transient", []byte("2"), false)

	var got []string
	unsubscribe, _ := m.Subscribe(ctx, "a/#", func(msg Message) { got = append(got, msg.Topic+"="+string(msg.Payload)) })
	_ = m.Publish(ctx, "a/new", []byte("3"), false)
	unsubscribe()
	_ = m.Publish(ctx, "a/after", []byte("4"), false)

	if len(got) != 2 || got[0] != "a/retained=1" || got[1] != "a/new=3" {
		t.Fatalf("got %v", got)
	}
	if s := m.Sent("a/new"); len(s) != 1 || s[0] != "3" {
		t.Fatalf("Sent = %v", s)
	}
}

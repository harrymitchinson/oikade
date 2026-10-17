/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
	_ = m.Subscribe(ctx, "a/#", func(msg Message) { got = append(got, msg.Topic+"="+string(msg.Payload)) })
	_ = m.Publish(ctx, "a/new", []byte("3"), false)

	if len(got) != 2 || got[0] != "a/retained=1" || got[1] != "a/new=3" {
		t.Fatalf("got %v", got)
	}
	if s := m.Sent("a/new"); len(s) != 1 || s[0] != "3" {
		t.Fatalf("Sent = %v", s)
	}
}

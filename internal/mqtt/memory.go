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
	"slices"
	"sync"
)

// Memory is an in-process Bus for tests. Delivery is synchronous; retained messages are kept.
type Memory struct {
	mu       sync.Mutex
	nextID   uint64
	subs     []subscription
	retained map[string][]byte
	// Published records every message in order.
	Published []Message
}

var _ Bus = (*Memory)(nil)

// NewMemory returns an empty in-memory bus.
func NewMemory() *Memory {
	return &Memory{retained: map[string][]byte{}}
}

// Publish implements Bus.
func (m *Memory) Publish(_ context.Context, topic string, payload []byte, retain bool) error {
	m.mu.Lock()
	msg := Message{Topic: topic, Payload: payload}
	m.Published = append(m.Published, Message{Topic: topic, Payload: payload, Retained: retain})
	if retain {
		m.retained[topic] = payload
	}
	subs := append([]subscription(nil), m.subs...)
	m.mu.Unlock()
	for _, s := range subs {
		if Match(s.filter, topic) {
			s.h(msg)
		}
	}
	return nil
}

// Subscribe implements Bus.
func (m *Memory) Subscribe(_ context.Context, filter string, h Handler) (func(), error) {
	m.mu.Lock()
	m.nextID++
	id := m.nextID
	m.subs = append(m.subs, subscription{id: id, filter: filter, h: h})
	var retained []Message
	for t, p := range m.retained {
		if Match(filter, t) {
			retained = append(retained, Message{Topic: t, Payload: p, Retained: true})
		}
	}
	m.mu.Unlock()
	for _, msg := range retained {
		h(msg)
	}
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.subs = slices.DeleteFunc(m.subs, func(s subscription) bool { return s.id == id })
	}, nil
}

// Sent returns the payloads published to topic, in order.
func (m *Memory) Sent(topic string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, msg := range m.Published {
		if msg.Topic == topic {
			out = append(out, string(msg.Payload))
		}
	}
	return out
}

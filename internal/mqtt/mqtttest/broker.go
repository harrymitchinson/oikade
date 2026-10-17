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

// Package mqtttest runs an in-process MQTT broker for tests.
package mqtttest

import (
	"log/slog"
	"net"
	"testing"

	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
)

// StartBroker starts a broker on a free localhost port and returns its mqtt:// URL.
// It is stopped when the test ends.
func StartBroker(t testing.TB) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	s := mochi.New(&mochi.Options{Logger: slog.New(slog.DiscardHandler)})
	if err := s.AddHook(new(auth.AllowHook), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.AddListener(listeners.NewTCP(listeners.Config{ID: "test", Address: addr})); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	t.Cleanup(func() { _ = s.Close() })
	return "mqtt://" + addr
}

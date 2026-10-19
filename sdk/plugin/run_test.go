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

package plugin

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/mqtt/mqtttest"
)

type handler struct {
	serve func(ctx context.Context, p *Plugin) error
}

func (h handler) Serve(ctx context.Context, p *Plugin) error { return h.serve(ctx, p) }
func (handler) Set(context.Context, *Plugin, contractv1.SetCommand) error {
	return nil
}

// setEnv sets the operator's environment for a plugin on url.
func setEnv(t *testing.T, url string) {
	t.Helper()
	t.Setenv(contractv1.EnvMQTTURL, url)
	t.Setenv(contractv1.EnvTopic, string(root))
	t.Setenv(contractv1.EnvDevicesFile, writeFile(t, t.TempDir(), "devices.json",
		`{"contract":1,"devices":[{"id":"relay-1","name":"relay","displayName":"Relay"}]}`))
}

func TestRunStopsCleanly(t *testing.T) {
	ctx := testContext(t)
	b := mqtttest.NewBroker(t)
	setEnv(t, b.URL)
	w := watch(ctx, t, b.URL)

	runCtx, stop := context.WithCancel(ctx)
	var declared []contractv1.DeclaredDevice
	code := make(chan int, 1)
	logs := &logBuffer{}
	go func() {
		code <- run(runCtx, slog.New(slog.NewJSONHandler(logs, nil)), Options{},
			func(_ Environment, devices []contractv1.DeclaredDevice) (Handler, error) {
				declared = devices
				return handler{serve: func(ctx context.Context, p *Plugin) error {
					// Serve may return early; Run keeps the plugin up until it's stopped.
					return p.SetDevices(ctx, []contractv1.DeviceReport{relay}, 0)
				}}, nil
			})
	}()
	w.expect(root.Status()+"="+online, root.Devices()+"="+relayDevices)
	stop() // as SIGTERM does
	if got := <-code; got != 0 {
		t.Fatalf("exited %d, want 0:\n%s", got, logs)
	}
	w.expect(root.Status() + "=" + offline)
	if len(declared) != 1 || declared[0].ID != "relay-1" {
		t.Fatalf("handler got %+v, want the devices file", declared)
	}
}

func TestRunFails(t *testing.T) {
	b := mqtttest.NewBroker(t)
	serveFails := func(Environment, []contractv1.DeclaredDevice) (Handler, error) {
		return handler{serve: func(context.Context, *Plugin) error { return errors.New("bus gone") }}, nil
	}
	for _, tc := range []struct {
		name       string
		env        func(t *testing.T)
		newHandler func(Environment, []contractv1.DeclaredDevice) (Handler, error)
		want       string
	}{
		{
			name: "missing environment",
			env:  func(t *testing.T) { setEnv(t, b.URL); t.Setenv(contractv1.EnvTopic, "") },
			want: `"msg":"Could not start plugin","error":"OIKADE_TOPIC is not set`,
		},
		{
			name: "bad devices file",
			env: func(t *testing.T) {
				setEnv(t, b.URL)
				t.Setenv(contractv1.EnvDevicesFile, writeFile(t, t.TempDir(), "d.json", `{"contract":2}`))
			},
			want: `contract 2 is not supported`,
		},
		{
			name: "handler refuses",
			env:  func(t *testing.T) { setEnv(t, b.URL) },
			newHandler: func(Environment, []contractv1.DeclaredDevice) (Handler, error) {
				return nil, errors.New("no LAN key")
			},
			want: `"msg":"Could not start plugin","error":"no LAN key"`,
		},
		{
			name:       "no broker",
			env:        func(t *testing.T) { setEnv(t, "mqtt://127.0.0.1:1") },
			newHandler: serveFails,
			want:       `"msg":"Could not start plugin","error":"could not come online`,
		},
		{
			name:       "serving fails",
			env:        func(t *testing.T) { setEnv(t, b.URL) },
			newHandler: serveFails,
			want:       `"msg":"Could not serve plugin","error":"bus gone"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.env(t)
			if tc.newHandler == nil {
				tc.newHandler = func(Environment, []contractv1.DeclaredDevice) (Handler, error) {
					t.Fatal("built a handler")
					return nil, nil
				}
			}
			logs := &logBuffer{}
			got := run(testContext(t), slog.New(slog.NewJSONHandler(logs, nil)),
				Options{StartTimeout: 200 * time.Millisecond}, tc.newHandler)
			if got != 1 || !strings.Contains(logs.String(), tc.want) {
				t.Fatalf("exited %d, logged:\n%s\nwant 1 and %s", got, logs, tc.want)
			}
		})
	}
}

package plugin

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"oikade.dev/contract/v1"
	"oikade.dev/internal/mqtt/mqtttest"
)

type handler struct {
	serve func(ctx context.Context, p *Plugin) error
}

func (h handler) Serve(ctx context.Context, p *Plugin) error { return h.serve(ctx, p) }
func (handler) Set(context.Context, *Plugin, contract.SetCommand) error {
	return nil
}

func setEnv(t *testing.T, url string) {
	t.Helper()
	t.Setenv(contract.EnvMQTTURL, url)
	t.Setenv(contract.EnvTopic, string(root))
	t.Setenv(contract.EnvDevicesFile, writeFile(t, t.TempDir(), "devices.json",
		`{"contract":1,"devices":[{"id":"relay-1","name":"relay","displayName":"Relay"}]}`))
}

func TestRunStopsCleanly(t *testing.T) {
	ctx := testContext(t)
	b := mqtttest.NewBroker(t)
	setEnv(t, b.URL)
	w := watchLive(ctx, t, b.URL)

	runCtx, stop := context.WithCancel(ctx)
	var declared []contract.DeclaredDevice
	code := make(chan int, 1)
	logs := &logBuffer{}
	go func() {
		code <- run(runCtx, slog.New(slog.NewJSONHandler(logs, nil)), Options{},
			func(_ Environment, devices []contract.DeclaredDevice) (Handler, error) {
				declared = devices
				return handler{serve: func(ctx context.Context, p *Plugin) error {
					// Serve may return early; Run keeps the plugin up until it's stopped.
					return p.SetDevices(ctx, []contract.DeviceReport{relay}, 0)
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
	serveFails := func(Environment, []contract.DeclaredDevice) (Handler, error) {
		return handler{serve: func(context.Context, *Plugin) error { return errors.New("bus gone") }}, nil
	}
	for _, tc := range []struct {
		name       string
		env        func(t *testing.T)
		newHandler func(Environment, []contract.DeclaredDevice) (Handler, error)
		want       string
	}{
		{
			name: "missing environment",
			env:  func(t *testing.T) { t.Helper(); setEnv(t, b.URL); t.Setenv(contract.EnvTopic, "") },
			want: `"msg":"Could not start plugin","err":"OIKADE_TOPIC is not set`,
		},
		{
			name: "bad devices file",
			env: func(t *testing.T) {
				t.Helper()
				setEnv(t, b.URL)
				t.Setenv(contract.EnvDevicesFile, writeFile(t, t.TempDir(), "d.json", `{"contract":2}`))
			},
			want: `contract 2 is not supported`,
		},
		{
			name: "handler refuses",
			env:  func(t *testing.T) { t.Helper(); setEnv(t, b.URL) },
			newHandler: func(Environment, []contract.DeclaredDevice) (Handler, error) {
				return nil, errors.New("no LAN key")
			},
			want: `"msg":"Could not start plugin","err":"no LAN key"`,
		},
		{
			name:       "no broker",
			env:        func(t *testing.T) { t.Helper(); setEnv(t, "mqtt://127.0.0.1:1") },
			newHandler: serveFails,
			want:       `"msg":"Could not start plugin","err":"could not come online`,
		},
		{
			name:       "serving fails",
			env:        func(t *testing.T) { t.Helper(); setEnv(t, b.URL) },
			newHandler: serveFails,
			want:       `"msg":"Could not serve plugin","err":"bus gone"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.env(t)
			if tc.newHandler == nil {
				tc.newHandler = func(Environment, []contract.DeclaredDevice) (Handler, error) {
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

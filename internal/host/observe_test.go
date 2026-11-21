package host

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VictoriaMetrics/metrics"

	"oikade.dev/internal/driver"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/hub"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/logging"
)

func TestTheHostExportsTheExistingMetricNames(t *testing.T) {
	exported := metrics.NewSet()
	store := hub.NewStore(logging.Discard())
	dev := ident.ID{Name: "fan"}
	inst := driver.InstanceKey{Driver: "test"}
	store.SetDevices(inst, map[ident.ID]hub.Meta{dev: {Instance: inst}})
	store.Changed(driver.Update{Device: dev, Capability: "on", Value: true, Time: time.Now()})
	store.Availability(dev, true)
	store.Seen(dev, time.Now())
	exported.GetOrCreateGauge(`oikade_device_control_level{capability="on",device="fan"}`, nil).Set(1)

	// Your process's own metrics, as they reach the host.
	process := metrics.NewSet()
	for _, name := range []string{
		`oikade_rule_runs_total{result="succeeded",rule="fan"}`, `oikade_rule_events_dropped_total`,
		`oikade_rule_budget_run_minutes_total{rule="fan"}`,
	} {
		process.GetOrCreateCounter(name).Inc()
	}
	for _, name := range []string{
		`oikade_rule_next_change_timestamp_seconds{rule="fan"}`, `oikade_rule_active{rule="fan"}`,
		`oikade_rule_budget_target_minutes{rule="fan"}`, `oikade_rule_budget_scheduled_minutes{rule="fan"}`,
		`oikade_rule_budget_today_run_minutes{rule="fan"}`, `oikade_rule_budget_desired{rule="fan"}`,
	} {
		process.GetOrCreateGauge(name, nil).Set(1)
	}
	var text bytes.Buffer
	process.WritePrometheus(&text)
	p := &pushed{}
	if err := p.set(bytes.TrimSuffix(text.Bytes(), []byte("\n"))); err != nil {
		t.Fatal(err)
	}
	export(exported, store, p)

	var out bytes.Buffer
	exported.WritePrometheus(&out)
	page := out.String()
	for _, want := range []string{
		`oikade_capability_value{capability="on",device="fan",room=""} 1`,
		"oikade_capability_changes_total{", "oikade_device_available{",
		"oikade_device_last_seen_timestamp_seconds{", `oikade_device_control_level{capability="on",device="fan"} 1`,
		"oikade_rule_runs_total{", "oikade_rule_next_change_timestamp_seconds{", "oikade_rule_active{",
		"oikade_rule_events_dropped_total 1", "oikade_rule_budget_target_minutes{",
		"oikade_rule_budget_scheduled_minutes{", "oikade_rule_budget_run_minutes_total{",
		"oikade_rule_budget_today_run_minutes{", `oikade_rule_budget_desired{rule="fan"} 1`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("no %s in\n%s", want, page)
		}
	}
	if !strings.HasSuffix(page, "\n") {
		t.Error("the last line isn't ended")
	}

	_ = p.set(nil)
	out.Reset()
	exported.WritePrometheus(&out)
	if strings.Contains(out.String(), "oikade_rule_runs_total") {
		t.Error("a process that's gone still has metrics")
	}
}

func TestTheHostRefusesMetricsThatArentRules(t *testing.T) {
	p := &pushed{}
	good := "# HELP oikade_rule_active\n# TYPE oikade_rule_active gauge\noikade_rule_active{rule=\"a b\"} 1\n"
	if err := p.set([]byte(good)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"oikade_capability_value{device=\"fan\"} 1\n",
		"oikade_rule_active{rule=\"fan} 1\n",
		"oikade_rule_active one\n",
		"# TYPE oikade_rule_active gaueg\n",
		"# TYPE oikade_device_control_level gauge\n",
		"oikade_rule_active\n",
	} {
		if err := p.set([]byte(good + bad)); err == nil {
			t.Errorf("kept %q", bad)
		}
		var out bytes.Buffer
		p.write(&out)
		if out.Len() != 0 {
			t.Errorf("served %q", out.String())
		}
		_ = p.set([]byte(good))
	}
	if err := p.set([]byte("nonsense 1\n")); err == nil {
		t.Error("kept nonsense")
	}
	if err := p.set([]byte("more nonsense 1\n")); err != nil {
		t.Errorf("said again why it refused a push: %v", err)
	}
}

func TestSocketsKeepOtherUsersOut(t *testing.T) {
	dir, err := os.MkdirTemp("", "oik") //nolint:usetesting // a Unix socket's path has to fit in about 100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, api.SocketName)
	l, err := listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func(l net.Listener) { _ = l.Close() }(l)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the socket's mode is %o", mode)
	}
}

func TestFeedCutsOffATailThatFellBehind(t *testing.T) {
	f := newFeed()
	events, stop := f.subscribe()
	defer stop()
	for range feedBuffer + 1 {
		f.publish(api.Event{Type: api.EventChange})
	}
	for range feedBuffer {
		<-events
	}
	if _, ok := <-events; ok {
		t.Error("a tail that fell behind stayed subscribed")
	}
}

package host

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"

	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
)

func TestTheHostExportsTheExistingMetricNames(t *testing.T) {
	eng := &engine.Engine{ControlLevel: engine.NewControlLevelGauge()}
	store := hub.NewStore(logging.Discard())
	dev := ident.ID{Namespace: "home", Name: "fan"}
	inst := driver.InstanceKey{Driver: "test"}
	store.SetDevices(inst, map[ident.ID]hub.Meta{dev: {Instance: inst}})
	store.Changed(driver.Update{Device: dev, Capability: "on", Value: true, Time: time.Now()})
	store.Availability(dev, true)
	store.Seen(dev, time.Now())
	eng.ControlLevel.WithLabelValues("home", "fan", "on").Set(1)

	// Your process's own registry, as its metrics reach the host.
	process := prometheus.NewRegistry()
	runs, next, dropped, active, budget := engine.NewRunsCounter(), engine.NewNextChangeGauge(),
		engine.NewDroppedCounter(), engine.NewActiveGauge(), engine.NewBudgetMetrics()
	process.MustRegister(runs, next, dropped, active)
	process.MustRegister(budget.Collectors()...)
	runs.WithLabelValues("home", "fan", "succeeded").Inc()
	next.WithLabelValues("home", "fan").Set(1)
	active.WithLabelValues("home", "fan").Set(1)
	budget.Target.WithLabelValues("home", "fan").Set(1)
	budget.Scheduled.WithLabelValues("home", "fan").Set(1)
	budget.Run.WithLabelValues("home", "fan").Inc()
	budget.TodayRun.WithLabelValues("home", "fan").Set(1)
	budget.Desired.WithLabelValues("home", "fan").Set(1)
	text, err := testMetricsText(process)
	if err != nil {
		t.Fatal(err)
	}
	p := &pushed{}
	p.set(text)

	families, err := gatherer(store, eng, p).Gather()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(families))
	for _, f := range families {
		names = append(names, f.GetName())
	}
	for _, want := range []string{
		"oikade_capability_value", "oikade_capability_changes_total", "oikade_device_available",
		"oikade_device_last_seen_timestamp_seconds", "oikade_device_control_level",
		"oikade_rule_runs_total", "oikade_rule_next_change_timestamp_seconds", "oikade_rule_active",
		"oikade_rule_events_dropped_total", "oikade_rule_budget_target_minutes",
		"oikade_rule_budget_scheduled_minutes", "oikade_rule_budget_run_minutes_total",
		"oikade_rule_budget_today_run_minutes", "oikade_rule_budget_desired",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("no %s in %v", want, names)
		}
	}

	p.set(nil)
	if families, err = gatherer(store, eng, p).Gather(); err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == "oikade_rule_runs_total" {
			t.Error("a process that's gone still has metrics")
		}
	}
}

func TestSocketsKeepOtherUsersOut(t *testing.T) {
	dir, err := os.MkdirTemp("", "oik")
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

func testMetricsText(g prometheus.Gatherer) ([]byte, error) {
	families, err := g.Gather()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	for _, f := range families {
		if _, err := expfmt.MetricFamilyToText(&out, f); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

package engine

import (
	vm "github.com/VictoriaMetrics/metrics"

	"oikade.dev/internal/control"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/series"
)

// The engine's metric families. docs/metrics.md says what each one is.
const (
	// runsFamily counts Rule trigger outcomes, by rule and result.
	runsFamily = "oikade_rule_runs_total"
	// nextChangeFamily is when a Rule's next at or sun trigger fires, or its hold next starts or
	// ends, as a Unix time.
	nextChangeFamily = "oikade_rule_next_change_timestamp_seconds"
	// droppedFamily counts capability changes the engine dropped because it fell behind.
	droppedFamily = "oikade_rule_events_dropped_total"
	// activeFamily is 1 while a Rule's hold is active, and 0 otherwise.
	activeFamily = "oikade_rule_active"
	// controlLevelFamily is the winning level of an arbitrated capability: 0 default, 1 Normal,
	// 2 Override, 3 Safety.
	controlLevelFamily = "oikade_device_control_level"
)

// engineMetrics are the engine's series, made by Init.
type engineMetrics struct {
	runs, nextChange, active series.Vec
	// dropped is nil without RuleMetrics.
	dropped      *vm.Counter
	budget       budgetMetrics
	controlLevel series.Vec
}

func newMetrics(rules, controls *vm.Set, rule func(ident.ID) string) engineMetrics {
	m := engineMetrics{
		runs:       series.NewVec(rules, runsFamily, labelRule, "result"),
		nextChange: series.NewVec(rules, nextChangeFamily, labelRule),
		active:     series.NewVec(rules, activeFamily, labelRule),
		budget: budgetMetrics{
			rule:      rule,
			target:    series.NewVec(rules, "oikade_rule_budget_target_minutes", labelRule),
			scheduled: series.NewVec(rules, "oikade_rule_budget_scheduled_minutes", labelRule),
			run:       series.NewVec(rules, "oikade_rule_budget_run_minutes_total", labelRule),
			missed:    series.NewVec(rules, "oikade_rule_budget_missed_minutes_total", labelRule),
			todayRun:  series.NewVec(rules, "oikade_rule_budget_today_run_minutes", labelRule),
			desired:   series.NewVec(rules, "oikade_rule_budget_desired", labelRule),
		},
		controlLevel: series.NewVec(controls, controlLevelFamily, "device", "capability"),
	}
	if rules != nil {
		m.dropped = rules.GetOrCreateCounter(droppedFamily) // at 0 from the start, for a rate to go up from
	}
	return m
}

// budgetMetrics are the budget holds' gauges and counter, by rule.
type budgetMetrics struct {
	// rule is a rule's label.
	rule func(ident.ID) string
	// target is a budget hold's minutes for the current planning day, and scheduled how many of them
	// the day's plan places.
	target, scheduled series.Vec
	// run is the minutes a budget hold's target has been on and available, and todayRun those in the
	// current planning day, to compare with target.
	run, todayRun series.Vec
	// missed is the minutes of a budget hold's planned blocks that went by with its target off while
	// its conditions were true.
	missed series.Vec
	// desired is 1 while a budget hold's plan wants its target on, and 0 otherwise.
	desired series.Vec
}

// ref is a rule's label: its ref, with its device named once rule can.
func (m budgetMetrics) ref(key ident.ID) string {
	if m.rule == nil {
		return control.RuleRef(key.Name).String()
	}
	return m.rule(key)
}

func (m budgetMetrics) all() []series.Vec {
	return []series.Vec{m.target, m.scheduled, m.run, m.todayRun, m.missed, m.desired}
}

// ruleRef is a rule's ref, as its series' rule label and its log lines name it, with its device
// named.
func (e *Engine) ruleRef(key ident.ID) string { return e.names().RuleRef(key.Name).String() }

// names are what the store calls the devices, for log lines and metric labels.
func (e *Engine) names() control.Names { return e.Store.Names() }

// name is a device's name, for a log line.
func (e *Engine) name(dev ident.ID) string { return e.names().Device(dev.Name) }

package engine

import "github.com/harrymitchinson/oikade/internal/series"

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

func (e *Engine) runCounts() series.Vec {
	return series.NewVec(e.RuleMetrics, runsFamily, labelRule, "result")
}

func (e *Engine) nextChange() series.Vec {
	return series.NewVec(e.RuleMetrics, nextChangeFamily, labelRule)
}

func (e *Engine) active() series.Vec { return series.NewVec(e.RuleMetrics, activeFamily, labelRule) }

func (e *Engine) controlLevel() series.Vec {
	return series.NewVec(e.ControlMetrics, controlLevelFamily, "device", "capability")
}

// budgetMetrics are the budget holds' gauges and counter, by rule.
type budgetMetrics struct {
	// target is a budget hold's minutes for the current planning day, and scheduled how many of them
	// the day's plan places.
	target, scheduled series.Vec
	// run is the minutes a budget hold's target has been on and available, and todayRun those in the
	// current planning day, to compare with target.
	run, todayRun series.Vec
	// desired is 1 while a budget hold's plan wants its target on, and 0 otherwise.
	desired series.Vec
}

func (e *Engine) budget() budgetMetrics {
	set := e.RuleMetrics
	return budgetMetrics{
		target:    series.NewVec(set, "oikade_rule_budget_target_minutes", labelRule),
		scheduled: series.NewVec(set, "oikade_rule_budget_scheduled_minutes", labelRule),
		run:       series.NewVec(set, "oikade_rule_budget_run_minutes_total", labelRule),
		todayRun:  series.NewVec(set, "oikade_rule_budget_today_run_minutes", labelRule),
		desired:   series.NewVec(set, "oikade_rule_budget_desired", labelRule),
	}
}

func (m budgetMetrics) all() []series.Vec {
	return []series.Vec{m.target, m.scheduled, m.run, m.todayRun, m.desired}
}

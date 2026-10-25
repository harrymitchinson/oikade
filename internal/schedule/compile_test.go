package schedule

import (
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
)

const drying = `
while:
  budget:
    slotMinutes: 30
    minutes:
      bands:
        input: {device: cellar-humidity, capability: sensor.relative-humidity, maxAge: 1h}
        missingMinutes: 180
        ranges:
        - {below: 55, minutes: 60}
        - {below: "65.5", minutes: 180}
        - {minutes: 360}
    prefer:
    - {window: {start: "23:00", end: "07:00"}, score: 3}
    - {window: {start: "12:00", end: "15:00", days: [Sat, Sun]}, score: -1}
    forbid:
    - {window: {start: "18:00", end: "22:00"}}
hold: {device: cellar-dehumidifier, capability: switch.on, value: true}
`

func budgetRule(t *testing.T, spec string) *homev1alpha1.Rule {
	t.Helper()
	var r homev1alpha1.Rule
	if err := yaml.UnmarshalStrict([]byte(spec), &r.Spec); err != nil {
		t.Fatal(err)
	}
	return &r
}

func dehumidifierControl() *homev1alpha1.CapabilityControl {
	starts := int32(4)
	return &homev1alpha1.CapabilityControl{
		Capability: "switch.on",
		MinimumOn:  &metav1.Duration{Duration: time.Hour}, MinimumOff: &metav1.Duration{Duration: 30 * time.Minute},
		MaximumStartsPerDay: &starts,
	}
}

func TestCompileToPlanInput(t *testing.T) {
	b, err := Compile(budgetRule(t, drying), dehumidifierControl())
	if err != nil {
		t.Fatal(err)
	}
	reading := &Reading{Value: 60, At: saturday.Add(-10 * time.Minute)}
	got := b.Input(2026, time.July, 4, saturday, paris, nil, func(device, capability string) *Reading {
		if device != "cellar-humidity" || capability != "sensor.relative-humidity" {
			t.Errorf("read(%s, %s), want the bands input", device, capability)
		}
		return reading
	})

	below55, below65 := 55.0, 65.5
	want := input(
		Rule{Name: "prefer[0]", Window: window("23:00", "07:00"), Effect: Prefer, Prefer: 3},
		Rule{Name: "prefer[1]", Window: window("12:00", "15:00", time.Saturday, time.Sunday), Effect: Prefer, Prefer: -1},
		Rule{Name: "forbid[0]", Window: window("18:00", "22:00"), Effect: Forbid},
	)
	want.Minutes = &Minutes{Bands: &Bands{
		Input: reading, MaxAge: time.Hour, MissingMinutes: 180,
		Ranges: []Band{{Below: &below55, Minutes: 60}, {Below: &below65, Minutes: 180}, {Minutes: 360}},
	}}
	want.Constraints = Constraints{MinimumRun: time.Hour, MinimumRest: 30 * time.Minute, MaximumStarts: 4}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Input() =\n%+v\nwant\n%+v", got, want)
	}

	day := Plan(got)
	if day.TargetSource != SourceBands || day.TargetMinutes != 180 || day.ShortfallMinutes != 0 {
		t.Errorf("Plan() = %s %d minutes, shortfall %d; want Bands 180, no shortfall", day.TargetSource, day.TargetMinutes, day.ShortfallMinutes)
	}
}

func TestCompileExpression(t *testing.T) {
	b, err := Compile(budgetRule(t, `
while:
  budget:
    planningTime: "06:30"
    slotMinutes: 15
    minutes: {expression: 'devices["cellar-humidity"]["sensor.relative-humidity"] > 60.0 ? 240 : 90'}
hold: {device: cellar-dehumidifier, capability: switch.on, value: true}
`), &homev1alpha1.CapabilityControl{Capability: "switch.on"})
	if err != nil {
		t.Fatal(err)
	}
	if b.PlanningMinute != 6*60+30 || b.SlotMinutes != 15 {
		t.Errorf("planning minute %d, slots %d; want 390, 15", b.PlanningMinute, b.SlotMinutes)
	}
	in := b.Input(2026, time.July, 4, saturday, paris, map[string]map[string]any{
		"cellar-humidity": {"sensor.relative-humidity": 72.0},
	}, nil)
	if n, err := in.Minutes.Expression(); n != 240 || err != nil {
		t.Errorf("Expression() = %d, %v; want 240", n, err)
	}
	if got := in.Slots[0].Start; !got.Equal(time.Date(2026, time.July, 4, 6, 30, 0, 0, paris)) {
		t.Errorf("first slot starts %s, want 06:30", got)
	}
	if in.Constraints != (Constraints{}) {
		t.Errorf("constraints %+v from an empty control entry", in.Constraints)
	}
}

func TestCompileErrors(t *testing.T) {
	const hold = "\nhold: {device: d, capability: switch.on, value: true}"
	for name, tc := range map[string]struct {
		spec    string
		control *homev1alpha1.CapabilityControl
		want    string
	}{
		"minimumOn off the slots": {drying, &homev1alpha1.CapabilityControl{
			MinimumOn: &metav1.Duration{Duration: 45 * time.Minute},
		}, "control minimumOn 45m0s isn't a multiple of budget.slotMinutes (30)"},
		"minimumOff off the slots": {drying, &homev1alpha1.CapabilityControl{
			MinimumOff: &metav1.Duration{Duration: 10 * time.Minute},
		}, "control minimumOff 10m0s isn't a multiple"},
		"no control entry": {drying, nil, "switch.on on cellar-dehumidifier has no control entry"},
		"both bands and expression": {`
while: {budget: {minutes: {expression: "60", bands: {input: {device: h, capability: sensor.temperature}, missingMinutes: 0, ranges: [{minutes: 1}]}}}}` + hold,
			nil, "exactly one of bands or expression"},
		"neither bands nor expression": {`
while: {budget: {minutes: {}}}` + hold, nil, "exactly one of bands or expression"},
		"bands out of order": {`
while: {budget: {minutes: {bands: {input: {device: h, capability: sensor.temperature}, missingMinutes: 0, ranges: [{below: 16, minutes: 1}, {below: 12, minutes: 2}, {minutes: 3}]}}}}` + hold,
			nil, "ranges[1]: below must be greater than the band before"},
		"an open band before the last": {`
while: {budget: {minutes: {bands: {input: {device: h, capability: sensor.temperature}, missingMinutes: 0, ranges: [{minutes: 1}, {below: 12, minutes: 2}]}}}}` + hold,
			nil, "ranges[0]: only the last band omits below"},
		"a bounded last band": {`
while: {budget: {minutes: {bands: {input: {device: h, capability: sensor.temperature}, missingMinutes: 0, ranges: [{below: 12, minutes: 2}]}}}}` + hold,
			nil, "ranges[0]: the last band omits below"},
		"a bool input": {`
while: {budget: {minutes: {bands: {input: {device: h, capability: sensor.contact-open}, missingMinutes: 0, ranges: [{minutes: 2}]}}}}` + hold,
			nil, `"sensor.contact-open" isn't a number capability`},
		"an expression that isn't an int": {`
while: {budget: {minutes: {expression: '"two hours"'}}}` + hold, nil, "must evaluate to an int"},
		"a sun window": {`
while: {budget: {minutes: {expression: "60"}, prefer: [{window: {start: {sun: Sunset}, end: "23:00"}, score: 1}]}}` + hold,
			nil, `budget.prefer[0].window: a budget window's start and end are "HH:MM", not sun times`},
		"a number target": {`
while: {budget: {minutes: {expression: "60"}}}
hold: {device: d, capability: light.level, value: 40}`, nil, "a budget needs a writable bool capability"},
		"held off": {`
while: {budget: {minutes: {expression: "60"}}}
hold: {device: d, capability: switch.on, value: false}`, nil, "a budget holds its capability at true"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Compile(budgetRule(t, tc.spec), tc.control)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Compile() = %v, want %q", err, tc.want)
			}
		})
	}
}

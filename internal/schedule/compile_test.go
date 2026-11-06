package schedule

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/control"
)

// drying is a dehumidifier's budget, by the cellar's humidity.
func drying() Spec {
	return Spec{
		Device: "cellar-dehumidifier", Capability: "switch.on", Value: true, SlotMinutes: 30,
		Bands: &BandSpec{
			Device: "cellar-humidity", Capability: "sensor.relative-humidity", MaxAge: time.Hour, MissingMinutes: 180,
			Ranges: []Band{{Below: new(55.0), Minutes: 60}, {Below: new(65.5), Minutes: 180}, {Minutes: 360}},
		},
		Prefer: []PreferSpec{
			{Window: WindowSpec{Start: "23:00", End: "07:00"}, Score: 3},
			{Window: WindowSpec{Start: "12:00", End: "15:00", Days: []time.Weekday{time.Saturday, time.Sunday}}, Score: -1},
		},
		Forbid: []WindowSpec{{Start: "18:00", End: "22:00"}},
	}
}

func dehumidifierControl() *control.Entry {
	return &control.Entry{
		Capability: "switch.on", MinimumOn: control.Duration(time.Hour), MinimumOff: control.Duration(30 * time.Minute),
		MaximumStartsPerDay: 4,
	}
}

func TestCompileToPlanInput(t *testing.T) {
	b, err := CompileSpec(drying(), dehumidifierControl())
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

func TestCompileMinutes(t *testing.T) {
	minutes := func(devices map[string]map[string]any, _ time.Time, _ *time.Location) (int, error) {
		if devices["cellar-humidity"]["sensor.relative-humidity"].(float64) > 60 {
			return 240, nil
		}
		return 90, nil
	}
	b, err := CompileSpec(Spec{
		Device: "cellar-dehumidifier", Capability: "switch.on", Value: true, PlanningTime: "06:30", SlotMinutes: 15, Minutes: minutes,
	}, &control.Entry{Capability: "switch.on"})
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
	in = b.Input(2026, time.July, 4, saturday, paris, map[string]map[string]any{
		"cellar-humidity": {"sensor.relative-humidity": 50.0},
	}, nil)
	if n, err := in.Minutes.Expression(); n != 90 || err != nil {
		t.Errorf("Expression() = %d, %v; want 90", n, err)
	}
	if got := in.Slots[0].Start; !got.Equal(time.Date(2026, time.July, 4, 6, 30, 0, 0, paris)) {
		t.Errorf("first slot starts %s, want 06:30", got)
	}
	if in.Constraints != (Constraints{}) {
		t.Errorf("constraints %+v from an empty control entry", in.Constraints)
	}
}

func TestCompileErrors(t *testing.T) {
	minutes := func(map[string]map[string]any, time.Time, *time.Location) (int, error) {
		return 0, errors.New("unused")
	}
	bands := func(capability string, ranges ...Band) Spec {
		return Spec{Device: "d", Capability: "switch.on", Value: true, Bands: &BandSpec{Device: "h", Capability: capability, Ranges: ranges}}
	}
	onMinutes := func(capability string, value any) Spec {
		return Spec{Device: "d", Capability: capability, Value: value, Minutes: minutes}
	}
	both := bands("sensor.temperature", Band{Minutes: 1})
	both.Minutes = minutes
	for name, tc := range map[string]struct {
		spec    Spec
		control *control.Entry
		want    string
	}{
		"minimumOn off the slots":  {drying(), &control.Entry{MinimumOn: control.Duration(45 * time.Minute)}, "control minimumOn 45m0s isn't a multiple of budget.slotMinutes (30)"},
		"minimumOff off the slots": {drying(), &control.Entry{MinimumOff: control.Duration(10 * time.Minute)}, "control minimumOff 10m0s isn't a multiple"},
		"no control entry":         {drying(), nil, "switch.on on cellar-dehumidifier has no control entry"},
		"both bands and minutes":   {both, nil, "exactly one of bands or minutes"},
		"neither":                  {Spec{Device: "d", Capability: "switch.on", Value: true}, nil, "exactly one of bands or minutes"},
		"bands out of order": {
			bands("sensor.temperature", Band{Below: new(16.0), Minutes: 1}, Band{Below: new(12.0), Minutes: 2}, Band{Minutes: 3}), nil,
			"ranges[1]: below must be greater than the band before",
		},
		"an open band before the last": {bands("sensor.temperature", Band{Minutes: 1}, Band{Below: new(12.0), Minutes: 2}), nil, "ranges[0]: only the last band omits below"},
		"a bounded last band":          {bands("sensor.temperature", Band{Below: new(12.0), Minutes: 2}), nil, "ranges[0]: the last band omits below"},
		"a bool input":                 {bands("sensor.contact-open", Band{Minutes: 2}), nil, `"sensor.contact-open" isn't a number capability`},
		"a number target":              {onMinutes("light.level", 40), nil, "a budget needs a writable bool capability"},
		"held off":                     {onMinutes("switch.on", false), nil, "a budget holds its capability at true"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := CompileSpec(tc.spec, tc.control)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("CompileSpec() = %v, want %q", err, tc.want)
			}
		})
	}
}

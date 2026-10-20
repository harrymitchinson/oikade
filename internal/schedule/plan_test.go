package schedule

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

var paris = mustLocation("Europe/Paris")

func mustLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// saturday is 2026-07-04, a Saturday in Paris.
var saturday = time.Date(2026, time.July, 4, 0, 0, 0, 0, paris)

func hm(s string) int {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		panic(err)
	}
	return h*60 + m
}

func window(start, end string, days ...time.Weekday) *Window {
	return &Window{Start: hm(start), End: hm(end), Days: days}
}

// input is a Saturday of 30-minute slots in Paris, planned at midnight.
func input(rules ...Rule) PlanInput {
	return PlanInput{
		Now:       saturday,
		NotBefore: saturday,
		Slots:     DaySlots(2026, time.July, 4, 0, 30, paris),
		Rules:     rules,
	}
}

func expression(minutes int) *Minutes {
	return &Minutes{Expression: func() (int, error) { return minutes, nil }}
}

// local formats blocks as local HH:MM-HH:MM for comparison.
//
//go:fix inline
func local(blocks []Block) string {
	s := make([]string, 0, len(blocks))
	for _, b := range blocks {
		s = append(s, b.Start.In(paris).Format("15:04")+"-"+b.End.In(paris).Format("15:04"))
	}
	return strings.Join(s, " ")
}

func TestBudget(t *testing.T) {
	bands := func(input *Reading) *Minutes {
		return &Minutes{Bands: &Bands{
			Input: input, MaxAge: 30 * time.Minute, MissingMinutes: 240,
			Ranges: []Band{{Below: new(12.0), Minutes: 120}, {Below: new(16.0), Minutes: 300}, {Minutes: 480}},
		}}
	}
	fresh := func(v float64) *Reading { return &Reading{Value: v, At: saturday.Add(-10 * time.Minute)} }
	cases := []struct {
		name    string
		minutes *Minutes
		source  TargetSource
		target  int
		err     bool
	}{
		{"below the first band", bands(fresh(8)), SourceBands, 120, false},
		{"at a threshold takes the next band", bands(fresh(12)), SourceBands, 300, false},
		{"negative values", bands(fresh(-2.5)), SourceBands, 120, false},
		{"the last band has no threshold", bands(fresh(30)), SourceBands, 480, false},
		{"missing input", bands(nil), SourceMissing, 240, false},
		{"stale input", bands(&Reading{Value: 8, At: saturday.Add(-31 * time.Minute)}), SourceMissing, 240, false},
		{"input exactly maxAge old", bands(&Reading{Value: 8, At: saturday.Add(-30 * time.Minute)}), SourceBands, 120, false},
		{"expression rounds up to a slot", expression(95), SourceExpression, 120, false},
		{"expression clamps below zero", expression(-5), SourceExpression, 0, false},
		{"expression clamps to a day", expression(5000), SourceExpression, 1440, false},
		{"expression error is zero", &Minutes{Expression: func() (int, error) { return 0, errors.New("no such key") }}, SourceExpression, 0, true},
		{"no budget", nil, SourceNone, 0, false},
		{"empty minutes is an error", &Minutes{}, SourceNone, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := input()
			in.Minutes = tc.minutes
			day := Plan(in)
			if day.TargetSource != tc.source || day.TargetMinutes != tc.target {
				t.Errorf("got %s %d, want %s %d", day.TargetSource, day.TargetMinutes, tc.source, tc.target)
			}
			if (len(day.Errors) > 0) != tc.err {
				t.Errorf("errors %v, want error %v", day.Errors, tc.err)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	// slotAt finds the slot starting at a local time on the Saturday.
	slotAt := func(slots []Slot, s string) int {
		for i, slot := range slots {
			if slot.Start.In(paris).Format("15:04") == s {
				return i
			}
		}
		t.Fatalf("no slot at %s", s)
		return -1
	}
	afternoon := func(s Slot) (bool, error) { return s.Start.In(paris).Hour() >= 12, nil }
	in := input(
		Rule{Name: "friday-night", Window: window("22:00", "06:00", time.Friday), Effect: Require},
		Rule{Name: "saturday-night", Window: window("23:00", "01:00", time.Saturday), Effect: Prefer, Prefer: 4},
		Rule{Name: "weekdays", Window: window("08:00", "09:00", time.Monday, time.Tuesday), Effect: Require},
		Rule{Name: "quiet", Window: window("05:00", "08:00"), Effect: Forbid},
		Rule{Name: "cheap", Window: window("07:00", "10:00"), Effect: Prefer, Prefer: 3},
		Rule{Name: "morning", Window: window("09:00", "11:00"), Effect: Prefer, Prefer: 2},
		Rule{Name: "avoid", Window: window("10:00", "10:15"), Effect: Prefer, Prefer: -10},
		Rule{Name: "slot-afternoon", When: afternoon, Effect: Prefer, Prefer: 1},
		Rule{Name: "broken", Window: window("20:00", "21:00"), When: func(Slot) (bool, error) { return true, errors.New("boom") }, Effect: Require},
	)
	in.NotBefore = saturday.Add(30 * time.Minute)
	// A second rule with the same name still reports its own error.
	in.Rules = append(in.Rules, in.Rules[len(in.Rules)-1])
	classes, errs := classify(in)

	cases := []struct {
		at   string
		want slotClass
	}{
		{"00:00", slotClass{kind: unavailable}},
		{"00:30", slotClass{kind: required}},  // Friday's window wraps into Saturday
		{"05:30", slotClass{kind: required}},  // require beats forbid
		{"06:00", slotClass{kind: forbidden}}, // Friday's window has ended
		{"07:30", slotClass{kind: forbidden}}, // forbid beats prefer
		{"08:00", slotClass{score: 3}},        // weekdays doesn't apply on Saturday
		{"09:30", slotClass{score: 5}},        // cheap and morning add up
		{"10:00", slotClass{score: -8}},       // a window covering part of the slot matches
		{"10:30", slotClass{score: 2}},
		{"11:30", slotClass{}},
		{"12:00", slotClass{score: 1}}, // when sees the slot
		{"20:00", slotClass{score: 1}}, // a failing condition is false
		{"22:00", slotClass{score: 1}}, // Friday's window doesn't start again on Saturday
		{"23:30", slotClass{score: 5}}, // Saturday's window covers the end of the day
	}
	for _, tc := range cases {
		if got := classes[slotAt(in.Slots, tc.at)]; got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.at, got, tc.want)
		}
	}
	if len(errs) != 2 || !strings.Contains(errs[0].Error(), `"broken"`) {
		t.Errorf("want one error for each broken rule, got %v", errs)
	}
}

func TestPlanRequiredOnly(t *testing.T) {
	in := input(
		Rule{Name: "off-peak", Window: window("22:00", "06:00"), Effect: Require},
		Rule{Name: "short", Window: window("12:00", "12:30"), Effect: Require},
	)
	// Constraints don't apply without a budget.
	in.Constraints = Constraints{MinimumRun: 2 * time.Hour, MinimumRest: 2 * time.Hour, MaximumStarts: 1}
	day := Plan(in)
	if got, want := local(day.Blocks), "00:00-06:00 12:00-12:30 22:00-00:00"; got != want {
		t.Errorf("blocks %q, want %q", got, want)
	}
	if day.TargetSource != SourceNone || day.TargetMinutes != 510 || day.ScheduledMinutes != 510 || day.ShortfallMinutes != 0 {
		t.Errorf("got %+v", day)
	}
}

func TestPlanConstraints(t *testing.T) {
	cases := []struct {
		name        string
		minutes     int
		rules       []Rule
		constraints Constraints
		lastOff     time.Time
		notBefore   time.Time
		want        string
	}{
		{
			name:    "fills the preferred window first",
			minutes: 180,
			rules:   []Rule{{Name: "cheap", Window: window("22:00", "06:00"), Effect: Prefer, Prefer: 3}},
			want:    "00:00-03:00",
		},
		{
			name:    "spills outside the preferred window",
			minutes: 240,
			rules: []Rule{
				{Name: "cheap", Window: window("02:00", "04:00"), Effect: Prefer, Prefer: 3},
				{Name: "quiet", Window: window("00:00", "01:00"), Effect: Forbid},
			},
			want: "01:00-05:00",
		},
		{
			name:        "minimum run joins short preferred windows",
			minutes:     120,
			rules:       []Rule{{Name: "a", Window: window("01:00", "01:30"), Effect: Prefer, Prefer: 5}, {Name: "b", Window: window("03:00", "03:30"), Effect: Prefer, Prefer: 5}},
			constraints: Constraints{MinimumRun: time.Hour, MaximumStarts: 2},
			want:        "00:30-01:30 02:30-03:30",
		},
		{
			name:    "minimum rest separates blocks",
			minutes: 120,
			rules: []Rule{
				{Name: "a", Window: window("01:00", "02:00"), Effect: Prefer, Prefer: 5},
				{Name: "b", Window: window("02:30", "03:30"), Effect: Prefer, Prefer: 5},
			},
			constraints: Constraints{MinimumRest: time.Hour},
			want:        "01:00-03:00",
		},
		{
			name:        "maximum starts keeps one block",
			minutes:     120,
			rules:       []Rule{{Name: "a", Window: window("01:00", "02:00"), Effect: Prefer, Prefer: 5}, {Name: "b", Window: window("05:00", "06:00"), Effect: Prefer, Prefer: 5}},
			constraints: Constraints{MaximumStarts: 1},
			want:        "00:00-02:00",
		},
		{
			name:        "required slots count towards the budget and may be shorter than the minimum run",
			minutes:     120,
			rules:       []Rule{{Name: "frost", Window: window("04:00", "04:30"), Effect: Require}, {Name: "late", Window: window("20:00", "22:00"), Effect: Prefer, Prefer: 1}},
			constraints: Constraints{MinimumRun: time.Hour, MinimumRest: time.Hour, MaximumStarts: 2},
			want:        "04:00-04:30 20:00-21:30",
		},
		{
			name:        "carried-over rest delays the first block",
			minutes:     60,
			constraints: Constraints{MinimumRest: 90 * time.Minute},
			lastOff:     saturday.Add(-30 * time.Minute),
			want:        "01:00-02:00",
		},
		{
			name:      "slots before notBefore are unavailable",
			minutes:   60,
			rules:     []Rule{{Name: "early", Window: window("00:00", "01:00"), Effect: Require}},
			notBefore: saturday.Add(15*time.Hour + 10*time.Minute),
			want:      "15:00-16:00",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := input(tc.rules...)
			in.Minutes = expression(tc.minutes)
			in.Constraints = tc.constraints
			in.LastOff = tc.lastOff
			if !tc.notBefore.IsZero() {
				in.NotBefore = tc.notBefore
			}
			day := Plan(in)
			if got := local(day.Blocks); got != tc.want {
				t.Errorf("blocks %q, want %q", got, tc.want)
			}
			classes, _ := classify(in)
			if v := violations(in, classes, day.TargetMinutes, day.Blocks); len(v) > 0 {
				t.Errorf("violations: %v", v)
			}
		})
	}
}

func TestPlanTieBreak(t *testing.T) {
	cases := []struct {
		name        string
		minutes     int
		rules       []Rule
		constraints Constraints
		want        string
	}{
		{
			name:    "equal scores take one early block",
			minutes: 90,
			want:    "00:00-01:30",
		},
		{
			name:    "score beats fewer starts",
			minutes: 60,
			rules:   []Rule{{Name: "a", Window: window("01:00", "01:30"), Effect: Prefer, Prefer: 1}, {Name: "b", Window: window("03:00", "03:30"), Effect: Prefer, Prefer: 1}},
			want:    "01:00-01:30 03:00-03:30",
		},
		{
			name:    "fewer starts beat earlier blocks",
			minutes: 60,
			rules:   []Rule{{Name: "a", Window: window("00:30", "02:00"), Effect: Forbid}, {Name: "b", Window: window("03:00", "00:00"), Effect: Forbid}},
			want:    "02:00-03:00",
		},
		{
			name:    "longer blocks before earlier blocks",
			minutes: 90,
			rules:   []Rule{{Name: "a", Window: window("01:00", "01:30"), Effect: Prefer, Prefer: 1}, {Name: "b", Window: window("03:00", "03:30"), Effect: Prefer, Prefer: 1}},
			want:    "00:30-01:30 03:00-03:30",
		},
		{
			name:    "longer blocks first at equal starts",
			minutes: 90,
			rules: []Rule{
				{Name: "gap", Window: window("01:00", "01:30"), Effect: Forbid},
				{Name: "rest", Window: window("02:30", "00:00"), Effect: Forbid},
			},
			// Allowed: 00:00-01:00 and 01:30-02:30, so the 90 minutes split into two blocks.
			want: "00:00-01:00 01:30-02:00",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := input(tc.rules...)
			in.Minutes = expression(tc.minutes)
			in.Constraints = tc.constraints
			first := Plan(in)
			if got := local(first.Blocks); got != tc.want {
				t.Errorf("blocks %q, want %q", got, tc.want)
			}
			for range 5 {
				if again := Plan(in); local(again.Blocks) != local(first.Blocks) {
					t.Fatalf("plan changed between runs: %q then %q", local(first.Blocks), local(again.Blocks))
				}
			}
		})
	}
}

func TestPlanShortfall(t *testing.T) {
	t.Run("constraints leave a shortfall", func(t *testing.T) {
		in := input(Rule{Name: "only", Window: window("06:00", "00:00"), Effect: Forbid})
		in.Minutes = expression(240)
		in.Constraints = Constraints{MinimumRun: time.Hour, MinimumRest: time.Hour, MaximumStarts: 2}
		day := Plan(in)
		if got := local(day.Blocks); got != "00:00-04:00" {
			t.Errorf("blocks %q", got)
		}
		in.Constraints.MaximumStarts = 1
		in.Rules = append(in.Rules, Rule{Name: "short", Window: window("02:00", "05:00"), Effect: Forbid})
		day = Plan(in)
		if day.ScheduledMinutes != 120 || day.ShortfallMinutes != 120 || local(day.Blocks) != "00:00-02:00" {
			t.Errorf("got %+v %q", day, local(day.Blocks))
		}
	})
	t.Run("required beyond the budget runs anyway", func(t *testing.T) {
		in := input(Rule{Name: "off-peak", Window: window("22:00", "06:00"), Effect: Require})
		in.Minutes = expression(120)
		day := Plan(in)
		if day.TargetMinutes != 120 || day.ScheduledMinutes != 480 || day.ShortfallMinutes != 0 {
			t.Errorf("got %+v", day)
		}
		if got := local(day.Blocks); got != "00:00-06:00 22:00-00:00" {
			t.Errorf("blocks %q", got)
		}
	})
	t.Run("required runs beyond maximum starts", func(t *testing.T) {
		in := input(
			Rule{Name: "a", Window: window("01:00", "02:00"), Effect: Require},
			Rule{Name: "b", Window: window("04:00", "05:00"), Effect: Require},
			Rule{Name: "c", Window: window("08:00", "09:00"), Effect: Require},
			Rule{Name: "cheap", Window: window("12:00", "14:00"), Effect: Prefer, Prefer: 5},
		)
		in.Minutes = expression(240)
		in.Constraints = Constraints{MaximumStarts: 2, MinimumRest: 30 * time.Minute}
		day := Plan(in)
		// No new start fits, so the budget extends the required blocks.
		if day.ScheduledMinutes != 240 || len(day.Blocks) != 3 {
			t.Errorf("got %+v %q", day, local(day.Blocks))
		}
	})
}

func TestPlanDST(t *testing.T) {
	cases := []struct {
		name     string
		day      int
		month    time.Month
		slots    int
		required string
		budget   string
	}{
		// 02:00 doesn't exist: the window from 01:00 to 04:00 lasts 2 hours.
		{"spring forward", 29, time.March, 46, "01:00-04:00", "04:00-05:00"},
		// 02:00 to 03:00 happens twice: the window lasts 4 hours.
		{"fall back", 25, time.October, 50, "01:00-04:00", "04:00-05:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slots := DaySlots(2026, tc.month, tc.day, 0, 30, paris)
			if len(slots) != tc.slots {
				t.Fatalf("got %d slots, want %d", len(slots), tc.slots)
			}
			start := slots[0].Start
			in := PlanInput{
				Now: start, NotBefore: start, Slots: slots,
				Rules: []Rule{
					{Name: "night", Window: window("01:00", "04:00"), Effect: Require},
					{Name: "cheap", Window: window("04:00", "06:00"), Effect: Prefer, Prefer: 1},
				},
			}
			day := Plan(in)
			if got := local(day.Blocks); got != tc.required {
				t.Errorf("required blocks %q, want %q", got, tc.required)
			}
			length := day.Blocks[0].End.Sub(day.Blocks[0].Start)
			if want := time.Duration(len(slots)-46)*30*time.Minute + 2*time.Hour; length != want {
				t.Errorf("required block lasts %s, want %s", length, want)
			}
			// The budget beyond the required block lands in the preferred hour after it.
			in.Minutes = expression(day.ScheduledMinutes + 60)
			in.Constraints = Constraints{MinimumRest: time.Hour}
			day = Plan(in)
			if got, want := local(day.Blocks), tc.required[:6]+tc.budget[6:]; got != want {
				t.Errorf("budget blocks %q, want %q", got, want)
			}
		})
	}
}

// randomInput makes a small day of hourly UTC slots with random rules and constraints.
func randomInput(r *rand.Rand, n int) PlanInput {
	start := time.Date(2026, time.July, 4, 0, 0, 0, 0, time.UTC)
	slots := make([]Slot, 0, n)
	for i := range n {
		slots = append(slots, Slot{Start: start.Add(time.Duration(i) * time.Hour), End: start.Add(time.Duration(i+1) * time.Hour)})
	}
	rules := make([]Rule, 0, 4)
	for i := range r.IntN(5) {
		s, e := r.IntN(24), r.IntN(24)
		if s == e {
			e = (e + 1) % 24
		}
		rule := Rule{Name: fmt.Sprint("r", i), Window: &Window{Start: s * 60, End: e * 60}}
		switch r.IntN(6) {
		case 0:
			rule.Effect = Require
		case 1:
			rule.Effect = Forbid
		default:
			rule.Effect, rule.Prefer = Prefer, r.IntN(9)-3
		}
		rules = append(rules, rule)
	}
	in := PlanInput{
		Now:       start,
		NotBefore: start.Add(time.Duration(r.IntN(3)) * time.Hour),
		Slots:     slots,
		Rules:     rules,
		Minutes:   expression(r.IntN(n+1) * 60),
		Constraints: Constraints{
			MinimumRun:    time.Duration(r.IntN(4)) * time.Hour,
			MinimumRest:   time.Duration(r.IntN(4)) * time.Hour,
			MaximumStarts: r.IntN(4),
		},
	}
	if r.IntN(2) == 0 {
		in.LastOff = start.Add(-time.Duration(r.IntN(3)) * time.Hour)
	}
	return in
}

// violations checks a plan against the rules the planner promises, independently of it.
func violations(in PlanInput, classes []slotClass, target int, plan []Block) []string {
	var v []string
	step := in.Slots[0].End.Sub(in.Slots[0].Start)
	on := make([]bool, len(in.Slots))
	for _, b := range plan {
		for i, s := range in.Slots {
			if !s.Start.Before(b.Start) && !s.End.After(b.End) {
				on[i] = true
			}
		}
	}
	count, req := 0, 0
	for i, c := range classes {
		if on[i] {
			count++
		}
		switch {
		case c.kind == required:
			req++
			if !on[i] {
				v = append(v, fmt.Sprintf("required slot %d is off", i))
			}
		case c.kind != allowed && on[i]:
			v = append(v, fmt.Sprintf("slot %d is on but %d", i, c.kind))
		}
	}
	if target /= int(step / time.Minute); count > max(target, req) {
		v = append(v, fmt.Sprintf("%d slots on, over the target %d", count, target))
	}
	return append(v, blockViolations(in, classes, on)...)
}

// blockViolations checks the constraints on blocks. Constraints only bind blocks that hold a
// slot the budget chose.
func blockViolations(in PlanInput, classes []slotClass, on []bool) []string {
	var v []string
	step := in.Slots[0].End.Sub(in.Slots[0].Start)
	type blk struct {
		start, end int
		fixed      bool
	}
	var bs []blk
	requiredRuns := 0
	for i := range on {
		if classes[i].kind == required && (i == 0 || classes[i-1].kind != required) {
			requiredRuns++
		}
		if !on[i] {
			continue
		}
		if len(bs) == 0 || bs[len(bs)-1].end != i {
			bs = append(bs, blk{start: i, end: i, fixed: true})
		}
		bs[len(bs)-1].end = i + 1
		bs[len(bs)-1].fixed = bs[len(bs)-1].fixed && classes[i].kind == required
	}
	for j, b := range bs {
		if !b.fixed && time.Duration(b.end-b.start)*step < in.Constraints.MinimumRun {
			v = append(v, fmt.Sprintf("block %d is shorter than the minimum run", j))
		}
		if j > 0 && (!b.fixed || !bs[j-1].fixed) && time.Duration(b.start-bs[j-1].end)*step < in.Constraints.MinimumRest {
			v = append(v, fmt.Sprintf("block %d starts before the minimum rest", j))
		}
		if j == 0 && !b.fixed && !in.LastOff.IsZero() && in.Slots[b.start].Start.Sub(in.LastOff) < in.Constraints.MinimumRest {
			v = append(v, "first block starts before the carried-over rest")
		}
	}
	if m := in.Constraints.MaximumStarts; m > 0 && len(bs) > max(m, requiredRuns) {
		v = append(v, fmt.Sprintf("%d starts, over the maximum %d", len(bs), m))
	}
	return v
}

func TestPlanProperties(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := range 2000 {
		in := randomInput(r, 24)
		day := Plan(in)
		classes, _ := classify(in)
		if v := violations(in, classes, day.TargetMinutes, day.Blocks); len(v) > 0 {
			t.Fatalf("case %d: %v\nrules %+v\nconstraints %+v\nblocks %v", i, v, in.Rules, in.Constraints, day.Blocks)
		}
		if day.ShortfallMinutes != max(0, day.TargetMinutes-day.ScheduledMinutes) {
			t.Fatalf("case %d: inconsistent minutes %+v", i, day)
		}
	}
}

// bruteForce tries every set of allowed slots and keeps the best valid plan, ranked as the
// planner ranks them.
func bruteForce(in PlanInput, classes []slotClass, target, extra int) [2]uint64 {
	var free []int
	var base plan
	for i, c := range classes {
		if c.kind == required {
			base.on[0] |= 1 << i
		}
		if c.kind == allowed {
			free = append(free, i)
		}
	}
	var best plan
	bestCount := -1
	for mask := range 1 << len(free) {
		p := base
		count := 0
		for j, i := range free {
			if mask&(1<<j) != 0 {
				p.on[0] |= 1 << i
				p.score += classes[i].score
				count++
			}
		}
		if count > extra {
			continue
		}
		plan := make([]Block, 0, len(classes))
		for _, b := range blocks(p.on, len(classes)) {
			plan = append(plan, Block{Start: in.Slots[b.start].Start, End: in.Slots[b.start+b.length-1].End})
		}
		p.starts = len(plan)
		if len(violations(in, classes, target, plan)) > 0 {
			continue
		}
		if count > bestCount || count == bestCount && better(p, best, len(classes)) {
			best, bestCount = p, count
		}
	}
	return best.on
}

func TestPlanOptimal(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	compared := 0
	for i := range 1000 {
		in := randomInput(r, 12)
		classes, _ := classify(in)
		day := Plan(in)
		req := 0
		for _, c := range classes {
			if c.kind == required {
				req++
			}
		}
		target := day.TargetMinutes / 60
		if target <= req {
			continue
		}
		compared++
		got, want := search(in, classes, target-req), bruteForce(in, classes, day.TargetMinutes, target-req)
		if got != want {
			t.Fatalf("case %d: planner %v, brute force %v\nclasses %+v\nconstraints %+v lastOff %v",
				i, slices.Collect(onSlots(got, 12)), slices.Collect(onSlots(want, 12)), classes, in.Constraints, in.LastOff)
		}
	}
	if compared < 300 {
		t.Errorf("only %d cases needed a search", compared)
	}
}

func BenchmarkPlanWorstCase(b *testing.B) {
	// 25 hours of 15-minute slots, unlimited starts and every slot scored.
	slots := DaySlots(2026, time.October, 25, 0, 15, paris)
	rules := make([]Rule, 0, 24)
	for h := range 24 {
		rules = append(rules, Rule{Name: fmt.Sprint("h", h), Window: &Window{Start: h * 60, End: (h + 1) % 24 * 60}, Effect: Prefer, Prefer: h%7 - 3})
	}
	in := PlanInput{
		Now: slots[0].Start, NotBefore: slots[0].Start, Slots: slots, Rules: rules,
		Minutes:     expression(720),
		Constraints: Constraints{MinimumRun: 2 * time.Hour, MinimumRest: 2 * time.Hour},
	}
	for b.Loop() {
		Plan(in)
	}
}

func onSlots(on [2]uint64, n int) func(func(int) bool) {
	return func(yield func(int) bool) {
		for i := range n {
			if on[i/64]&(1<<(i%64)) != 0 && !yield(i) {
				return
			}
		}
	}
}

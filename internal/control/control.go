// Package control arbitrates one capability of a device. Rules, Matter controllers, oikade
// commands and the device itself make claims on it. The arbiter picks the winner by level, then by
// the decision's order of lines, then the latest claim, and holds the winner back by the device's
// equipment limits.
//
// A decision's lines map onto levels: a line above the override line is Safety, an outside change
// is Override, and a line below it is Normal. So the level is what a safety line is, and Lines
// order the claims within one.
//
// Values are float64. A bool capability uses 0 for false and 1 for true. For the limits, any
// non-zero value is on.
package control

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

type Kind int

const (
	Bool Kind = iota + 1
	Number
	// Name is one of Settings.Names names, held as its number: 0 up to, not including, Names.
	Name
)

// Level orders claims. A higher level always wins.
type Level int

const (
	// LevelDefault is the device's default value, when nobody claims.
	LevelDefault Level = iota
	Normal
	Override
	Safety
)

func (l Level) String() string {
	switch l {
	case LevelDefault:
		return "Default"
	case Normal:
		return "Normal"
	case Override:
		return "Override"
	case Safety:
		return "Safety"
	}
	return fmt.Sprintf("Level(%d)", int(l))
}

// External says what an outside change does.
type External int

const (
	// ExternalOverride makes an outside change an Override claim for the device's override hold.
	ExternalOverride External = iota + 1
	// ExternalRevert writes the winner straight back.
	ExternalRevert
)

// Limit names the equipment limit holding the winner back.
type Limit int

const (
	NoLimit Limit = iota
	MinimumOn
	MaximumOn
	MinimumOff
	MaximumStartsPerDay
)

func (l Limit) String() string {
	switch l {
	case NoLimit:
		return ""
	case MinimumOn:
		return "MinimumOn"
	case MaximumOn:
		return "MaximumOn"
	case MinimumOff:
		return "MinimumOff"
	case MaximumStartsPerDay:
		return "MaximumStartsPerDay"
	}
	return fmt.Sprintf("Limit(%d)", int(l))
}

type Settings struct {
	Kind Kind
	// Names is how many names a Name has.
	Names   int
	Default float64
	// Lines are the claims' sources in the decision's order, highest first, with LineOverride for
	// the override line and LineFlows for the flows'. A claim from a rule that isn't a line ranks
	// at LineFlows, or after every line without one.
	Lines    []string
	External External
	// OverrideHold is how long an outside change holds as a claim, unless OverrideUntilNext.
	OverrideHold time.Duration
	// OverrideUntilNext ends an outside change's claim when the result without it next changes.
	OverrideUntilNext bool
	// MaximumOn stops the device after it has been on this long. It then rests for MinimumOff,
	// or, without a MinimumOff, until the winner is off.
	MinimumOn, MaximumOn, MinimumOff time.Duration
	// MaximumStartsPerDay limits off-to-on transitions per local day. Zero means unlimited.
	MaximumStartsPerDay int
	// Location is where a day starts for MaximumStartsPerDay. Nil means UTC.
	Location *time.Location
}

// Validate reports settings the arbiter can't act on.
func (s Settings) Validate() error {
	var errs []error
	switch s.Kind {
	case Bool:
		if s.Default != 0 && s.Default != 1 {
			errs = append(errs, errors.New("default must be 0 or 1 for a bool capability"))
		}
	case Number:
	case Name:
		if !s.isName(s.Default) {
			errs = append(errs, fmt.Errorf("default %v isn't one of the %d names", s.Default, s.Names))
		}
		// A limit counts the time a device is on, which a name has no part in.
		if s.MinimumOn != 0 || s.MaximumOn != 0 || s.MinimumOff != 0 || s.MaximumStartsPerDay != 0 {
			errs = append(errs, errors.New("limits need a bool, and this capability is one of some names"))
		}
	default:
		errs = append(errs, fmt.Errorf("unknown kind %d", s.Kind))
	}
	switch s.External {
	case ExternalOverride:
		if s.OverrideHold <= 0 && !s.OverrideUntilNext {
			errs = append(errs, errors.New("external Override needs an override hold"))
		}
	case ExternalRevert:
	default:
		errs = append(errs, fmt.Errorf("unknown external %d", s.External))
	}
	if s.MinimumOn < 0 || s.MaximumOn < 0 || s.MinimumOff < 0 || s.OverrideHold < 0 || s.MaximumStartsPerDay < 0 {
		errs = append(errs, errors.New("limits and the override hold must not be negative"))
	}
	if s.MaximumOn > 0 && s.MaximumOn < s.MinimumOn {
		errs = append(errs, errors.New("maximumOn must not be shorter than minimumOn"))
	}
	return errors.Join(errs...)
}

// Claim is one source's claim on the capability. A source has at most one claim.
type Claim struct {
	// Source is the path the claim came in by: "Rule/<key>", "Matter", "Device" or "Command".
	Source string  `json:"source"`
	Level  Level   `json:"level"`
	Value  float64 `json:"value"`
	// Expires ends the claim at this time. Zero means no expiry.
	Expires time.Time `json:"expires,omitzero"`
	// UntilNext ends the claim when the result without it next differs from Baseline. With
	// Expires as well, the claim ends at whichever comes first.
	UntilNext bool `json:"untilNext,omitempty"`
	// Baseline is the result without this claim when it was made. The arbiter sets it.
	Baseline float64 `json:"baseline,omitempty"`
	// External marks a claim made from an outside change. The arbiter sets it.
	External bool `json:"external,omitempty"`
	// Latched is shown in status only. A latched hold ends with Release, like any other.
	Latched bool `json:"latched,omitempty"`
}

// Revert records an outside change that was written back.
type Revert struct {
	Source string    `json:"source"`
	Value  float64   `json:"value"`
	At     time.Time `json:"at"`
	// From is the device's value before the change.
	From float64 `json:"from"`
}

// State is everything the arbiter remembers. It is what must survive a restart.
type State struct {
	Claims []Claim `json:"claims,omitempty"`
	// Known says whether Value is the device's actual value.
	Known bool    `json:"known,omitempty"`
	Value float64 `json:"value,omitempty"`
	// LastOn and LastOff are when the device last turned on and off. Zero means long ago.
	LastOn  time.Time `json:"lastOn,omitzero"`
	LastOff time.Time `json:"lastOff,omitzero"`
	// Starts counts off-to-on transitions on the local day starting at Day.
	Day    time.Time `json:"day,omitzero"`
	Starts int       `json:"starts,omitempty"`
	// Tripped is set when MaximumOn stopped the device and there is no MinimumOff to rest for.
	Tripped bool `json:"tripped,omitempty"`
	// Reverting is the outside change under ExternalRevert waiting to be written back.
	Reverting *Revert `json:"reverting,omitempty"`
	// Reverted is the last outside change that was written back.
	Reverted *Revert `json:"reverted,omitempty"`
}

func (s State) clone() State {
	s.Claims = slices.Clone(s.Claims)
	s.Reverting, s.Reverted = cloneRevert(s.Reverting), cloneRevert(s.Reverted)
	return s
}

func cloneRevert(r *Revert) *Revert {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

// Decision is what the device should be, and why.
type Decision struct {
	Value float64
	// Write is set when Value isn't the device's known value.
	Write bool
	// Wanted is the winner's value, before the limits.
	Wanted float64
	// Level and Source name the winning claim. Source is empty for the default.
	Level  Level
	Source string
	// Limit is the limit holding Wanted back, if any.
	Limit Limit
	// Next is the earliest time the decision can change without new input. Zero means never.
	Next time.Time
	// Claims are the live claims, highest level first.
	Claims   []Claim
	Reverted *Revert
}

// Clock tells an arbiter the time. An arbiter never waits: it says when it next needs to run.
type Clock interface{ Now() time.Time }

// Arbiter arbitrates one capability. It isn't safe for concurrent use.
type Arbiter struct {
	settings Settings
	clock    Clock
	st       State
}

// New returns an arbiter that continues from st, which may be the zero State.
func New(s Settings, c Clock, st State) (*Arbiter, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if s.Location == nil {
		s.Location = time.UTC
	}
	return &Arbiter{settings: s, clock: c, st: st.clone()}, nil
}

// State returns a copy of what the arbiter remembers, for persisting.
func (a *Arbiter) State() State { return a.st.clone() }

func (a *Arbiter) checkValue(v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("value %v is not a finite number", v)
	}
	if a.settings.Kind == Bool && v != 0 && v != 1 {
		return fmt.Errorf("value %v is not 0 or 1", v)
	}
	if a.settings.Kind == Name && !a.settings.isName(v) {
		return fmt.Errorf("value %v is not one of the %d names", v, a.settings.Names)
	}
	return nil
}

// isName reports whether v is one of a Name's numbers.
func (s Settings) isName(v float64) bool {
	return v == math.Trunc(v) && v >= 0 && v < float64(s.Names)
}

// Claim adds c, replacing any claim from the same source.
func (a *Arbiter) Claim(c Claim) error {
	if c.Source == "" {
		return errors.New("claim has no source")
	}
	if c.Level < Normal || c.Level > Safety {
		return fmt.Errorf("claim level %v is not Normal, Override or Safety", c.Level)
	}
	if err := a.checkValue(c.Value); err != nil {
		return err
	}
	if l := a.settings.Lines; len(l) > 0 && !slices.Contains(l, c.Source) && !slices.Contains(l, LineFlows) {
		return fmt.Errorf("%s isn't a line of the decision, which has no Flows line for it", c.Source)
	}
	c.External, c.Baseline = false, 0
	a.add(c)
	return nil
}

func (a *Arbiter) add(c Claim) {
	now := a.clock.Now()
	a.drop(c.Source)
	// Expired claims mustn't count towards the baseline.
	a.settle(now)
	c.Baseline, _, _ = a.winner(-1)
	a.st.Claims = append(a.st.Claims, c)
	a.settle(now)
}

// Release ends the source's claim, if it has one.
func (a *Arbiter) Release(source string) {
	a.drop(source)
	a.settle(a.clock.Now())
}

func (a *Arbiter) drop(source string) {
	a.st.Claims = slices.DeleteFunc(a.st.Claims, func(c Claim) bool { return c.Source == source })
}

// Wrote records that the device now has the value v because the arbiter's decision was written.
func (a *Arbiter) Wrote(v float64) {
	if r := a.st.Reverting; r != nil {
		a.st.Reverting = nil
		if v == r.From {
			// The write back undoes the outside change, so neither counts towards the limits.
			a.st.Value = v
			return
		}
	}
	a.record(v)
}

// Outside records a change the arbiter didn't decide, from source ("Matter", "Device" or
// "Command"). It counts towards the limits and isn't fought by them. Under ExternalOverride it
// replaces any earlier outside claim with an Override claim. Under ExternalRevert, the next
// decision writes the device's earlier value back, exempt from the limits, if that is still the
// winner, and otherwise the change is accepted. A value the device already has is no change.
func (a *Arbiter) Outside(source string, v float64) error {
	if source == "" {
		return errors.New("outside change has no source")
	}
	if err := a.checkValue(v); err != nil {
		return err
	}
	if a.st.Known && v == a.st.Value {
		return nil
	}
	now := a.clock.Now()
	if a.settings.External == ExternalRevert && a.st.Known {
		r := &Revert{Source: source, Value: v, At: now, From: a.st.Value}
		if a.st.Reverting != nil {
			r.From = a.st.Reverting.From
		}
		a.st.Reverting, a.st.Value = r, v
		return nil
	}
	a.record(v)
	if a.settings.External == ExternalRevert {
		return nil
	}
	a.st.Claims = slices.DeleteFunc(a.st.Claims, func(c Claim) bool { return c.External })
	c := Claim{Source: source, Level: Override, Value: v, External: true, UntilNext: a.settings.OverrideUntilNext}
	if !c.UntilNext {
		c.Expires = now.Add(a.settings.OverrideHold)
	}
	a.add(c)
	return nil
}

// record updates the device's value and the limit counters.
func (a *Arbiter) record(v float64) {
	now := a.clock.Now()
	a.rollDay(now)
	wasOn := a.st.Known && a.st.Value != 0
	switch {
	case v != 0 && !wasOn:
		// A device first seen on started at an unknown time, so it isn't counted as a start
		// today, and its run counts from now.
		a.st.LastOn = now
		if a.st.Known {
			a.st.Starts++
		}
	case v == 0 && wasOn:
		a.st.LastOff = now
	}
	a.st.Value, a.st.Known = v, true
}

func (a *Arbiter) rollDay(now time.Time) {
	if day := a.day(now); !day.Equal(a.st.Day) {
		a.st.Day, a.st.Starts = day, 0
	}
}

func (a *Arbiter) day(t time.Time) time.Time {
	y, m, d := t.In(a.settings.Location).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, a.settings.Location)
}

// Decide ends claims that are due and returns what the device should be.
func (a *Arbiter) Decide() Decision {
	now := a.clock.Now()
	a.settle(now)
	a.rollDay(now)
	wanted, level, source := a.winner(-1)
	d := Decision{Wanted: wanted, Level: level, Source: source}
	if r := a.st.Reverting; r != nil && wanted != r.From {
		// The winner has moved on from the value the change undid, so there is nothing to
		// restore: the change stands and counts, and the limits apply as usual.
		a.st.Reverting, a.st.Value = nil, r.From
		a.record(r.Value)
	}
	if r := a.st.Reverting; r != nil {
		// Writing the earlier value back restores the device, so it is exempt from the limits.
		d.Value = wanted
		a.st.Reverted = cloneRevert(r)
	} else {
		d.Value, d.Limit, d.Next = a.limit(now, wanted)
	}
	for _, c := range a.st.Claims {
		if !c.Expires.IsZero() && (d.Next.IsZero() || c.Expires.Before(d.Next)) {
			d.Next = c.Expires
		}
	}
	d.Write = !a.st.Known || a.st.Value != d.Value
	// Highest first, and within a tie the later claim first, as it wins.
	d.Claims = slices.Clone(a.st.Claims)
	slices.Reverse(d.Claims)
	slices.SortStableFunc(d.Claims, func(x, y Claim) int {
		if x.Level != y.Level {
			return int(y.Level - x.Level)
		}
		return a.Rank(x) - a.Rank(y)
	})
	d.Reverted = cloneRevert(a.st.Reverted)
	return d
}

// winner picks among the claims, leaving out the claim at index skip. Claims are kept in the order
// they came, so of two on the same line the later wins.
func (a *Arbiter) winner(skip int) (float64, Level, string) {
	best := -1
	for i, c := range a.st.Claims {
		if i == skip {
			continue
		}
		if best < 0 || !a.beats(a.st.Claims[best], c) {
			best = i
		}
	}
	if best < 0 {
		return a.settings.Default, LevelDefault, ""
	}
	c := a.st.Claims[best]
	return c.Value, c.Level, c.Source
}

// beats reports whether c outranks o: a higher level, or the same level and an earlier line.
func (a *Arbiter) beats(c, o Claim) bool {
	if c.Level != o.Level {
		return c.Level > o.Level
	}
	return a.Rank(c) < a.Rank(o)
}

// Rank is the claim's line in the decision, from 0 at the top. An outside change ranks at the
// override line, a rule with no line of its own at the flows' line, and anything else after every
// line.
func (a *Arbiter) Rank(c Claim) int {
	lines := a.settings.Lines
	at := func(line string) int {
		if i := slices.Index(lines, line); i >= 0 {
			return i
		}
		return len(lines)
	}
	if c.External {
		return at(LineOverride)
	}
	if i := slices.Index(lines, c.Source); i >= 0 {
		return i
	}
	return at(LineFlows)
}

// settle ends expired claims and until-next claims whose result without them has changed,
// until none is left to end, since ending one can change another's result.
func (a *Arbiter) settle(now time.Time) {
	for {
		i := slices.IndexFunc(a.st.Claims, func(c Claim) bool {
			return !c.Expires.IsZero() && !now.Before(c.Expires)
		})
		if i < 0 {
			for j, c := range a.st.Claims {
				if v, _, _ := a.winner(j); c.UntilNext && v != c.Baseline {
					i = j
					break
				}
			}
		}
		if i < 0 {
			return
		}
		a.st.Claims = slices.Delete(a.st.Claims, i, i+1)
	}
}

// limit holds wanted back by the equipment limits. It returns the value to write, the limit
// holding it and when that limit lets go.
func (a *Arbiter) limit(now time.Time, wanted float64) (float64, Limit, time.Time) {
	s := a.settings
	if !a.st.Known {
		return wanted, NoLimit, time.Time{}
	}
	on := wanted != 0
	if a.st.Value != 0 {
		var next time.Time
		if s.MaximumOn > 0 {
			end := a.st.LastOn.Add(s.MaximumOn)
			if !now.Before(end) {
				if !on {
					return wanted, NoLimit, time.Time{}
				}
				a.st.Tripped = s.MinimumOff == 0
				return 0, MaximumOn, time.Time{}
			}
			next = end
		}
		if on {
			return wanted, NoLimit, next
		}
		// Validate keeps minimumOn within maximumOn, so this hold ends first.
		if until := a.st.LastOn.Add(s.MinimumOn); now.Before(until) {
			return a.st.Value, MinimumOn, until
		}
		return wanted, NoLimit, time.Time{}
	}
	if !on {
		a.st.Tripped = false
		return wanted, NoLimit, time.Time{}
	}
	if until := a.st.LastOff.Add(s.MinimumOff); now.Before(until) {
		return 0, MinimumOff, until
	}
	if a.st.Tripped {
		return 0, MaximumOn, time.Time{}
	}
	if s.MaximumStartsPerDay > 0 && a.st.Starts >= s.MaximumStartsPerDay {
		return 0, MaximumStartsPerDay, a.day(now).AddDate(0, 0, 1)
	}
	return wanted, NoLimit, time.Time{}
}

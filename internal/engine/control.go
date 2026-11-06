package engine

import (
	"errors"
	"fmt"
	"time"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
)

// ControlSettings compiles a Device's control entry into arbiter settings. Errors name the
// capability, so the operator can report them on the Device as they are. loc is where a day
// starts for maximumStartsPerDay.
func ControlSettings(c control.Entry, loc *time.Location) (control.Settings, error) {
	s, err := controlSettings(c, loc)
	if err != nil {
		return control.Settings{}, fmt.Errorf("control %s: %w", c.Capability, err)
	}
	return s, nil
}

func controlSettings(c control.Entry, loc *time.Location) (control.Settings, error) {
	spec, ok := capability.Lookup(c.Capability)
	if !ok {
		return control.Settings{}, errors.New("unknown capability")
	}
	if !spec.Writable {
		return control.Settings{}, errors.New("capability is read-only")
	}
	s := control.Settings{Location: loc}
	switch spec.Kind {
	case homev1alpha1.CapabilityKindBool:
		s.Kind = control.Bool
	default:
		s.Kind = control.Number
	}

	var errs []error
	if def, err := capability.ParseJSON(c.Capability, c.Default); err != nil {
		errs = append(errs, fmt.Errorf("default: %w", err))
	} else {
		s.Default = capability.Float(def)
	}
	switch c.Resolve {
	case control.NamePreferOn:
		s.Resolve = control.On
	case control.NamePreferOff:
		s.Resolve = control.Off
	case control.NameHighest:
		s.Resolve = control.Highest
	case control.NameLowest:
		s.Resolve = control.Lowest
	}
	// Checked here rather than by the arbiter, so the message uses the API's names.
	if s.Kind == control.Bool && s.Resolve != control.On && s.Resolve != control.Off {
		errs = append(errs, errors.New("resolve must be PreferOn or PreferOff for a bool capability"))
	}
	if s.Kind == control.Number && s.Resolve != control.Highest && s.Resolve != control.Lowest {
		errs = append(errs, errors.New("resolve must be Highest or Lowest for a number capability"))
	}
	switch c.External {
	case control.NameOverride:
		s.External = control.ExternalOverride
	case control.NameRevert:
		s.External = control.ExternalRevert
	}
	switch c.OverrideHold {
	case "":
	case control.NameNext:
		s.OverrideUntilNext = true
	default:
		d, err := time.ParseDuration(c.OverrideHold)
		if err != nil {
			errs = append(errs, fmt.Errorf("overrideHold: %w", err))
		}
		s.OverrideHold = d
	}
	s.MinimumOn = time.Duration(c.MinimumOn)
	s.MaximumOn = time.Duration(c.MaximumOn)
	s.MinimumOff = time.Duration(c.MinimumOff)
	s.MaximumStartsPerDay = c.MaximumStartsPerDay
	if len(errs) == 0 {
		// The kind, default and resolve are settled, so the arbiter's own checks report the rest.
		errs = append(errs, s.Validate())
	}
	return s, errors.Join(errs...)
}

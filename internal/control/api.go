package control

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
)

// FromAPI is a Device's control entry as an Entry.
func FromAPI(c homev1alpha1.CapabilityControl) Entry {
	e := Entry{
		Capability: c.Capability, Default: c.Default.Raw, Resolve: string(c.Resolve), External: string(c.External),
		OverrideHold: c.OverrideHold, MinimumOn: duration(c.MinimumOn), MaximumOn: duration(c.MaximumOn),
		MinimumOff: duration(c.MinimumOff),
	}
	if c.MaximumStartsPerDay != nil {
		e.MaximumStartsPerDay = int(*c.MaximumStartsPerDay)
	}
	return e
}

// FromAPIList is a Device's control entries as Entries.
func FromAPIList(list []homev1alpha1.CapabilityControl) []Entry {
	if list == nil {
		return nil
	}
	out := make([]Entry, 0, len(list))
	for _, c := range list {
		out = append(out, FromAPI(c))
	}
	return out
}

func duration(d *metav1.Duration) Duration {
	if d == nil {
		return 0
	}
	return Duration(d.Duration)
}

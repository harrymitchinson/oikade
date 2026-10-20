/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package engine

import (
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/control"
)

func controlEntry(capability, def string, resolve homev1alpha1.ControlResolve) homev1alpha1.CapabilityControl {
	return homev1alpha1.CapabilityControl{
		Capability: capability, Default: apiextensionsv1.JSON{Raw: []byte(def)},
		Resolve: resolve, External: homev1alpha1.ControlExternalRevert,
	}
}

func TestControlSettingsBool(t *testing.T) {
	c := controlEntry("switch.on", "true", homev1alpha1.ControlResolvePreferOff)
	c.External, c.OverrideHold = homev1alpha1.ControlExternalOverride, "2h"
	c.MinimumOn, c.MaximumOn = &metav1.Duration{Duration: time.Hour}, &metav1.Duration{Duration: 6 * time.Hour}
	c.MinimumOff, c.MaximumStartsPerDay = &metav1.Duration{Duration: 30 * time.Minute}, ptr.To[int32](4)

	got, err := ControlSettings(c, london)
	if err != nil {
		t.Fatal(err)
	}
	want := control.Settings{
		Kind: control.Bool, Default: 1, Resolve: control.Off, External: control.ExternalOverride,
		OverrideHold: 2 * time.Hour, MinimumOn: time.Hour, MaximumOn: 6 * time.Hour, MinimumOff: 30 * time.Minute,
		MaximumStartsPerDay: 4, Location: london,
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestControlSettingsNumberUntilNext(t *testing.T) {
	c := controlEntry("light.level", "40", homev1alpha1.ControlResolveHighest)
	c.External, c.OverrideHold = homev1alpha1.ControlExternalOverride, homev1alpha1.OverrideHoldNext

	got, err := ControlSettings(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != control.Number || got.Default != 40 || got.Resolve != control.Highest || !got.OverrideUntilNext {
		t.Errorf("got %+v", got)
	}
}

func TestControlSettingsRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		entry homev1alpha1.CapabilityControl
		want  string
	}{
		"unknown capability": {controlEntry("light.hue", "1", homev1alpha1.ControlResolveHighest), "unknown capability"},
		"read-only":          {controlEntry("sensor.temperature", "20", homev1alpha1.ControlResolveHighest), "read-only"},
		"number resolve on a bool": {
			controlEntry("light.on", "false", homev1alpha1.ControlResolveHighest), "resolve must be PreferOn or PreferOff",
		},
		"bool resolve on a number": {
			controlEntry("light.level", "0", homev1alpha1.ControlResolvePreferOn), "resolve must be Highest or Lowest",
		},
		"default of the wrong kind": {controlEntry("light.on", "1", homev1alpha1.ControlResolvePreferOn), "takes a bool"},
		"default out of range":      {controlEntry("light.level", "150", homev1alpha1.ControlResolveLowest), "between 0 and 100"},
		"override without a hold": {func() homev1alpha1.CapabilityControl {
			c := controlEntry("switch.on", "false", homev1alpha1.ControlResolvePreferOn)
			c.External = homev1alpha1.ControlExternalOverride
			return c
		}(), "needs an override hold"},
		"maximumOn shorter than minimumOn": {func() homev1alpha1.CapabilityControl {
			c := controlEntry("switch.on", "false", homev1alpha1.ControlResolvePreferOn)
			c.MinimumOn, c.MaximumOn = &metav1.Duration{Duration: time.Hour}, &metav1.Duration{Duration: time.Minute}
			return c
		}(), "maximumOn must not be shorter than minimumOn"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ControlSettings(tc.entry, nil)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), "control "+tc.entry.Capability+": ") {
				t.Errorf("got %q, want it to name the capability and contain %q", err, tc.want)
			}
		})
	}
}

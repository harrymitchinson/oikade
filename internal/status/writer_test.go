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

package status

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
)

func TestComputeResolved(t *testing.T) {
	zigbee := homev1alpha1.DeviceSpec{Zigbee2MQTT: &homev1alpha1.ZigbeeDeviceSource{IEEEAddress: "0x01"}}
	plugin := homev1alpha1.DeviceSpec{Plugin: &homev1alpha1.PluginDeviceSource{ID: "relay"}}
	schedule := homev1alpha1.DeviceSpec{Schedule: &homev1alpha1.ScheduleDeviceSource{}}
	long := strings.Repeat("é", 200)

	for _, tc := range []struct {
		name         string
		spec         homev1alpha1.DeviceSpec
		snap         hub.Snapshot
		known        bool
		status       metav1.ConditionStatus
		reason       string
		message      string // substring
		capabilities int
	}{
		{name: "no zigbee driver", spec: zigbee, status: metav1.ConditionFalse, reason: "NoDriver", message: "spec.zigbee2mqtt.instanceRef"},
		{name: "no plugin driver", spec: plugin, status: metav1.ConditionFalse, reason: "NoDriver", message: "spec.plugin.pluginRef"},
		{name: "no schedule driver", spec: schedule, status: metav1.ConditionFalse, reason: "NoDriver", message: "spec.schedule.scheduleRef"},
		{name: "waiting", spec: plugin, known: true, status: metav1.ConditionUnknown, reason: "Waiting"},
		{
			name: "zigbee not paired", spec: zigbee, known: true, snap: hub.Snapshot{Discovered: true},
			status: metav1.ConditionFalse, reason: "NotPaired", message: "not paired with",
		},
		{
			name: "plugin not found", spec: plugin, known: true, snap: hub.Snapshot{Discovered: true},
			status: metav1.ConditionFalse, reason: "NotPaired", message: "has not found the device",
		},
		{
			name: "plugin error", spec: plugin, known: true,
			snap:   hub.Snapshot{Discovered: true, Info: driver.Info{Error: "no LAN key for relay"}},
			status: metav1.ConditionFalse, reason: "PluginError", message: "no LAN key for relay",
		},
		{
			name: "plugin error on a found device keeps capabilities", spec: plugin, known: true,
			snap:   hub.Snapshot{Discovered: true, Info: driver.Info{Found: true, Capabilities: []string{capability.SwitchOn}, Error: "bad parameters"}},
			status: metav1.ConditionFalse, reason: "PluginError", message: "bad parameters", capabilities: 1,
		},
		{
			name: "paired", spec: plugin, known: true,
			snap:   hub.Snapshot{Discovered: true, Info: driver.Info{Found: true, Capabilities: []string{capability.SwitchOn}}},
			status: metav1.ConditionTrue, reason: "Paired", capabilities: 1,
		},
		{
			name: "long error", spec: plugin, known: true,
			snap:   hub.Snapshot{Discovered: true, Info: driver.Info{Error: long}},
			status: metav1.ConditionFalse, reason: "PluginError", message: "é…",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &homev1alpha1.Device{Spec: tc.spec}
			compute(d, tc.snap, tc.known, time.Minute)
			c := meta.FindStatusCondition(d.Status.Conditions, homev1alpha1.ConditionResolved)
			if c == nil {
				t.Fatal("no Resolved condition")
			}
			if c.Status != tc.status || c.Reason != tc.reason || !strings.Contains(c.Message, tc.message) {
				t.Errorf("Resolved = %s %s %q, want %s %s containing %q", c.Status, c.Reason, c.Message, tc.status, tc.reason, tc.message)
			}
			if len(c.Message) > maxErrorLen || !utf8.ValidString(c.Message) {
				t.Errorf("message is %d bytes or invalid UTF-8", len(c.Message))
			}
			if got := len(d.Status.Capabilities); got != tc.capabilities {
				t.Errorf("capabilities = %d, want %d", got, tc.capabilities)
			}
		})
	}
}

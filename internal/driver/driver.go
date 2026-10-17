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

// Package driver defines how device integrations report to the runtime.
package driver

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// Info is what a driver knows about a declared device.
type Info struct {
	// Found is false while the device is declared but not paired with the driver.
	Found        bool
	Capabilities []string
	Manufacturer string
	Model        string
}

// Update is a capability changing value. Drivers only report actual changes.
type Update struct {
	Device     types.NamespacedName
	Capability string
	// Value is bool or float64, validated against the capability registry.
	Value any
	// Previous is nil the first time a value is seen.
	Previous any
	Time     time.Time
}

// Sink receives driver events. Calls arrive on the driver's receive goroutine and must not block.
type Sink interface {
	Discovered(dev types.NamespacedName, info Info)
	Changed(u Update)
	Availability(dev types.NamespacedName, online bool)
	// Seen reports any message from the device, changed or not.
	Seen(dev types.NamespacedName, t time.Time)
	// Undeclared reports how many paired devices have no Device object, per driver instance.
	Undeclared(instance types.NamespacedName, count int)
}

// Setter writes capabilities.
type Setter interface {
	Set(ctx context.Context, dev types.NamespacedName, capability string, value any) error
}

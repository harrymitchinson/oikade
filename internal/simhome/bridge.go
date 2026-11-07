package simhome

// FromFramework turns a framework.Home into a Home. The framework fills it in, because its compiled
// form is internal, and nothing outside it can reach that without being in its public API.
var FromFramework func(home any) (Home, error)

// FromSet reads a framework.Set as one device's value, for a test to report, and fails for a room's
// or one with a claim's options. The framework fills it in, as it does FromFramework.
var FromSet func(set any) (device, capability string, value any, err error)

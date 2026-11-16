package simhome

// FromHome turns an oikade.Home into a Home. Package oikade fills it in, because its compiled
// form is internal, and nothing outside it can reach that without being in its public API.
var FromHome func(home any) (Home, error)

// FromSet reads an oikade.Set as one device's value, for a test to report, and fails for a room's
// or one with a claim's options. Package oikade fills it in, as it does FromHome.
var FromSet func(set any) (device, capability string, value any, err error)

package simhome

// FromFramework turns a framework.Home into a Home. The framework fills it in, because its compiled
// form is internal, and nothing outside it can reach that without being in its public API.
var FromFramework func(home any) (Home, error)

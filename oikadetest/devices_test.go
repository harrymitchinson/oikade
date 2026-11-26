package oikadetest_test

import "oikade.dev"

// Your home's rooms and devices. oikade types adds each device you pair that isn't here yet, and
// never changes a line that is: rename, move and retype them here.

var (
	Hall = oikade.Device[struct {
		oikade.OccupancySensor
		Humidity oikade.Number
	}]("hall")
	HallFan     = oikade.Device[oikade.OnOffPlugInUnit]("hall-fan")
	Lounge      = oikade.Device[oikade.OnOffLight]("lounge")
	PorchLight  = oikade.Device[oikade.OnOffLight]("porch-light")
	PorchSensor = oikade.Device[oikade.OccupancySensor]("porch-sensor")
)

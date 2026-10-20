// Package sun computes sunrise and sunset locally, with NOAA's solar position equations. They're
// good to about a minute away from the poles, which is what published tables round to.
package sun

import (
	"math"
	"time"
)

// Event is a sun event.
type Event int

const (
	Sunrise Event = iota + 1
	Sunset
)

// Site is where the sun is observed from, in degrees. North and east are positive.
type Site struct {
	Latitude, Longitude float64
}

// zenith is the sun's centre at sunrise and sunset: 90° plus 50′ for refraction and the sun's
// radius, as published tables use.
const zenith = 90.833

// Time returns when e happens on the local calendar day year-month-day in loc. A day's events
// are the ones around its solar noon, so a sunset just after midnight belongs to the day before.
// It reports false when the sun doesn't rise or set that day.
func (s Site) Time(e Event, year int, month time.Month, day int, loc *time.Location) (time.Time, bool) {
	noon := s.solarNoon(time.Date(year, month, day, 12, 0, 0, 0, loc))
	sign := -1.0
	if e == Sunset {
		sign = 1
	}
	// Refine once at the event's own time: declination and the equation of time move a little
	// over half a day.
	t := noon
	for range 2 {
		ha, ok := s.hourAngle(t)
		if !ok {
			return time.Time{}, false
		}
		t = s.solarNoon(t).Add(time.Duration(sign * ha * 4 * float64(time.Minute)))
	}
	return t.In(loc).Truncate(time.Second), true
}

// solarNoon returns the solar noon nearest to near.
func (s Site) solarNoon(near time.Time) time.Time {
	utc := near.UTC()
	midnight := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	_, eqTime := position(centuries(near))
	noon := midnight.Add(time.Duration((720 - 4*s.Longitude - eqTime) * float64(time.Minute)))
	switch d := noon.Sub(near); {
	case d > 12*time.Hour:
		noon = noon.Add(-24 * time.Hour)
	case d < -12*time.Hour:
		noon = noon.Add(24 * time.Hour)
	}
	return noon
}

// hourAngle is the sun's hour angle at sunrise or sunset, in degrees, with the declination at t.
func (s Site) hourAngle(t time.Time) (float64, bool) {
	decl, _ := position(centuries(t))
	lat := rad(s.Latitude)
	cos := math.Cos(rad(zenith))/(math.Cos(lat)*math.Cos(decl)) - math.Tan(lat)*math.Tan(decl)
	if cos < -1 || cos > 1 {
		return 0, false
	}
	return deg(math.Acos(cos)), true
}

// centuries is the time since J2000.0 in Julian centuries.
func centuries(t time.Time) float64 {
	jd := float64(t.Unix())/86400 + 2440587.5
	return (jd - 2451545) / 36525
}

// position returns the sun's declination in radians and the equation of time in minutes.
func position(t float64) (float64, float64) {
	l0 := math.Mod(280.46646+t*(36000.76983+t*0.0003032), 360)
	m := 357.52911 + t*(35999.05029-0.0001537*t)
	e := 0.016708634 - t*(0.000042037+0.0000001267*t)
	c := math.Sin(rad(m))*(1.914602-t*(0.004817+0.000014*t)) +
		math.Sin(rad(2*m))*(0.019993-0.000101*t) + math.Sin(rad(3*m))*0.000289
	omega := 125.04 - 1934.136*t
	appLong := l0 + c - 0.00569 - 0.00478*math.Sin(rad(omega))
	obliq := 23 + (26+(21.448-t*(46.815+t*(0.00059-t*0.001813)))/60)/60 + 0.00256*math.Cos(rad(omega))
	decl := math.Asin(math.Sin(rad(obliq)) * math.Sin(rad(appLong)))
	y := math.Pow(math.Tan(rad(obliq/2)), 2)
	eqTime := 4 * deg(y*math.Sin(2*rad(l0))-2*e*math.Sin(rad(m))+
		4*e*y*math.Sin(rad(m))*math.Cos(2*rad(l0))-
		0.5*y*y*math.Sin(4*rad(l0))-1.25*e*e*math.Sin(2*rad(m)))
	return decl, eqTime
}

func rad(d float64) float64 { return d * math.Pi / 180 }
func deg(r float64) float64 { return r * 180 / math.Pi }

package cli

import (
	"fmt"
	"regexp"
	"time"
)

// Ago says when t was, from now: relative near now (8 s ago, 12 min ago, today 08:45, yesterday
// 21:10, 3 days ago), and the date and time past a week or over a minute ahead, in now's time zone.
func Ago(t, now time.Time) string {
	t = t.In(now.Location())
	d := now.Sub(t)
	switch {
	case d < -time.Minute:
		return t.Format("2006-01-02 15:04")
	case d < time.Second:
		// A minute ahead is the host's clock and this one's disagreeing.
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%d s ago", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	}
	days := daysBetween(t, now)
	switch {
	case days == 0:
		return "today " + t.Format("15:04")
	case days == 1:
		return "yesterday " + t.Format("15:04")
	case days <= 7:
		return fmt.Sprintf("%d days ago", days)
	}
	return t.Format("2006-01-02 15:04")
}

// daysBetween is how many midnights there are from t to now, in now's time zone.
func daysBetween(t, now time.Time) int {
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	// Noon keeps a daylight saving change from moving a day.
	from := time.Date(y1, m1, d1, 12, 0, 0, 0, time.UTC)
	to := time.Date(y2, m2, d2, 12, 0, 0, 0, time.UTC)
	return int(to.Sub(from) / (24 * time.Hour))
}

// hash is a version that's a commit's or a build's hash, rather than a release's.
var hash = regexp.MustCompile(`^[0-9a-f]{12,}$`)

// ShortVersion is v as a person reads it: a hash's first 7 characters (f113018), and a release's
// version as it is (v0.2.0-alpha.2).
func ShortVersion(v string) string {
	if hash.MatchString(v) {
		return v[:7]
	}
	return v
}

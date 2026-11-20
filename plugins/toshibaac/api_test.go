package toshibaac

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestTheWaitALimitAsksForIsRead(t *testing.T) {
	now := time.Date(2026, 11, 20, 17, 6, 15, 0, time.UTC)
	for _, c := range []struct {
		name, retryAfter, body string
		want                   time.Duration
		asked                  bool
	}{
		{name: "the login's body", body: `{"message":"Too many requests. Try again in 60 seconds."}`, want: time.Minute, asked: true},
		{name: "one second", body: `try again in 1 second`, want: time.Second, asked: true},
		{name: "Retry-After in seconds", retryAfter: "120", want: 2 * time.Minute, asked: true},
		{name: "Retry-After ahead of the body", retryAfter: "30", body: "Try again in 60 seconds.", want: 30 * time.Second, asked: true},
		{name: "Retry-After as a date", retryAfter: "Fri, 20 Nov 2026 17:07:00 GMT", want: 45 * time.Second, asked: true},
		{name: "Retry-After as a date gone by", retryAfter: "Fri, 20 Nov 2026 17:00:00 GMT", want: 0, asked: true},
		{name: "Retry-After of 0", retryAfter: "0", want: 0, asked: true},
		{name: "a Retry-After that isn't one, and the body", retryAfter: "soon", body: "Try again in 60 seconds.", want: time.Minute, asked: true},
		{name: "the firewall's 403", body: "<html>Forbidden</html>"},
		{name: "a wait too long to count", body: "Try again in 99999999999 seconds."},
		{name: "nothing"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			if c.retryAfter != "" {
				h.Set("Retry-After", c.retryAfter)
			}
			got, asked := askedWait(h, []byte(c.body), now)
			if got != c.want || asked != c.asked {
				t.Errorf("askedWait = %s, %t, not %s, %t", got, asked, c.want, c.asked)
			}
		})
	}
}

func TestALimitWaitsBetweenRetryAndLimited(t *testing.T) {
	b := &bridge{timing: cloudTiming}
	limited := func(wait time.Duration, asked bool) error {
		return fmt.Errorf("%w: %w", errLimited, &statusError{path: loginPath, code: http.StatusTooManyRequests, wait: wait, asked: asked})
	}
	for _, c := range []struct {
		name string
		err  error
		last time.Duration
		want time.Duration
	}{
		{name: "as asked", err: limited(time.Minute, true), want: time.Minute},
		{name: "twice the last, if that's longer", err: limited(time.Minute, true), last: time.Minute, want: 2 * time.Minute},
		{name: "as asked, if that's longer than twice the last", err: limited(5*time.Minute, true), last: time.Minute, want: 5 * time.Minute},
		{name: "no sooner than retry", err: limited(0, true), want: cloudTiming.retry},
		{name: "no longer than limited", err: limited(time.Hour, true), want: cloudTiming.limited},
		{name: "twice the last, no longer than limited", err: limited(time.Minute, true), last: 8 * time.Minute, want: cloudTiming.limited},
		{name: "limited, if it doesn't say", err: limited(0, false), want: cloudTiming.limited},
		{name: "limited, if it isn't an answer", err: errLimited, want: cloudTiming.limited},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := b.limitWait(c.err, c.last); got != c.want {
				t.Errorf("limitWait = %s, not %s", got, c.want)
			}
		})
	}
}

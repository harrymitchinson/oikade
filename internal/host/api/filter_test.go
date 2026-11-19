package api

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A filter comes back from its query as it went in, and one that narrows nothing has no query.
func TestAnEventFilterGoesInAQuery(t *testing.T) {
	if q := (EventFilter{}).Query(); len(q) != 0 {
		t.Errorf("a filter that narrows nothing is %q", q.Encode())
	}
	at := time.Date(2026, 11, 18, 3, 0, 0, 500, time.FixedZone("CET", 3600))
	want := EventFilter{Since: at, Until: at.Add(time.Hour), Refs: []string{"device/porch-light/on", "hardware/overkiz/io:%2F%2F1"}}
	got, err := ParseEventFilter(want.Query())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Since.Equal(want.Since) || !got.Until.Equal(want.Until) || !reflect.DeepEqual(got.Refs, want.Refs) {
		t.Errorf("%+v came back as %+v", want, got)
	}
}

// A query that can't be read says which part, and what one looks like.
func TestAnEventFilterThatCantBeReadSaysWhy(t *testing.T) {
	at := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	backwards := EventFilter{Since: at, Until: at.Add(-time.Hour)}.Query()
	for query, want := range map[string]string{
		"since=yesterday":   `since="yesterday" isn't a time`,
		"until=03:00":       `until="03:00" isn't a time`,
		backwards.Encode():  "until is before since",
		"since=":            `since="" isn't a time`,
		"since=1763434800":  "isn't a time",
		"ref=a&until=later": `until="later" isn't a time`,
	} {
		q, err := url.ParseQuery(query)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseEventFilter(q); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", query, err, want)
		}
	}
}

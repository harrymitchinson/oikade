package hue

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func readAll(t *testing.T, stream string) ([]string, error) {
	t.Helper()
	r := newEventReader(strings.NewReader(stream))
	var got []string
	for {
		data, err := r.next()
		if err != nil {
			return got, err
		}
		got = append(got, string(data))
	}
}

func TestTheEventReaderReadsEachEventsData(t *testing.T) {
	stream := ": hi\n\n" +
		"id: 1795514874:0\ndata: [{\"type\":\"update\"}]\n\n" +
		// CRLF, a field it doesn't use, a data line with no space, and two that join.
		"id: 1795514875:0\r\nevent: message\r\ndata:[1,\r\ndata: 2]\r\n\r\n" +
		"\n\n" +
		"data\n\n"
	got, err := readAll(t, stream)
	if !errors.Is(err, io.EOF) {
		t.Errorf("ended with %v, want EOF", err)
	}
	want := []string{`[{"type":"update"}]`, "[1,\n2]", ""}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTheEventReaderSaysAStreamEndedPartway(t *testing.T) {
	got, err := readAll(t, "data: [1]\n\ndata: [2")
	if !errors.Is(err, io.ErrUnexpectedEOF) || len(got) != 1 {
		t.Errorf("got %q and %v, want one event and ErrUnexpectedEOF", got, err)
	}
}

func TestTheEventReaderRefusesAnEventTooLong(t *testing.T) {
	long := strings.Repeat("x", maxEvent/2)
	if _, err := readAll(t, "data: "+long+"\ndata: "+long+"\ndata: "+long+"\n\n"); !errors.Is(err, errEventTooLong) {
		t.Errorf("got %v, want errEventTooLong", err)
	}
	if _, err := readAll(t, "data: "+strings.Repeat("y", maxEvent+10)+"\n\n"); !errors.Is(err, errEventTooLong) {
		t.Errorf("a long line: got %v, want errEventTooLong", err)
	}
}

func TestAnErrorFromTheBridgeSaysWhatItSaid(t *testing.T) {
	e := &apiError{status: 429, description: "too many requests"}
	if got, want := e.Error(), "the bridge answered 429 Too Many Requests: too many requests"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if !limitedRequests(e) || refusedKey(e) {
		t.Error("a 429 is a limit, not a refused key")
	}
	if !refusedKey(&apiError{status: 403}) || !refusedKey(&apiError{status: 401}) {
		t.Error("a 401 and a 403 are a refused key")
	}
}

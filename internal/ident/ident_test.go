package ident

import (
	"strings"
	"testing"
)

func TestStringIsTheStoreKey(t *testing.T) {
	if got := (ID{Name: "fan"}).String(); got != "fan" {
		t.Fatalf("String() = %q", got)
	}
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"desk-plug": true, "a": true, "room2": true, "a-1-b": true, strings.Repeat("a", MaxName): true,
		"": false, "Desk": false, "2nd-floor": false, "a--b": false, "a-": false, "-a": false,
		"a_b": false, "a.b": false, "a b": false, "a/b": false, strings.Repeat("a", MaxName+1): false,
	} {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestSlugMakesAValidName(t *testing.T) {
	for _, c := range []struct {
		kind     Kind
		in, want string
	}{
		{Room, "Living Room", "living-room"},
		{Room, "  Kid's  room! ", "kid-s-room"},
		{Room, "2nd floor", "room-2nd-floor"},
		{Device, "0x00158d", "device-0x00158d"},
		{Room, "Café", "caf"},
		{Room, "—", ""},
		{Room, "", ""},
		{Flow, strings.Repeat("ab-", 40), strings.TrimRight(strings.Repeat("ab-", 21), "-")},
	} {
		got := Slug(c.kind, c.in)
		if got != c.want {
			t.Errorf("Slug(%s, %q) = %q, want %q", c.kind, c.in, got, c.want)
		}
		if got != "" && !ValidName(got) {
			t.Errorf("Slug(%s, %q) = %q, which isn't a valid name", c.kind, c.in, got)
		}
	}
}

func TestCheckNameSuggestsAName(t *testing.T) {
	if err := CheckName(Room, "living-room"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		kind       Kind
		name, want string
	}{
		{Room, "Living Room", `"Living Room" isn't a room name: ` + Rule + `, such as "living-room"`},
		{Flow, "good_night", `"good_night" isn't a flow name: ` + Rule + `, such as "good-night"`},
		{Secret, "!!", `"!!" isn't a secret name: ` + Rule},
		{Group, "", "a group needs a name"},
	} {
		err := CheckName(c.kind, c.name)
		if err == nil || err.Error() != c.want {
			t.Errorf("CheckName(%s, %q) = %v, want %s", c.kind, c.name, err, c.want)
		}
	}
}

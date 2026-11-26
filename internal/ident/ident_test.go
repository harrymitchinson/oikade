package ident

import (
	"log/slog"
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
		{Device, "Entrée détecteur", "entree-detecteur"},
		{Room, "Salle à manger", "salle-a-manger"},
		{Device, "Terrasse sud côté garage", "terrasse-sud-cote-garage"},
		{Room, "Çà et là", "ca-et-la"},
		{Room, "Ñandú", "nandu"},
		{Room, "Ærø Œuvre", "aero-oeuvre"},
		{Room, "Straße", "strasse"},
		{Room, "Łódź", "lodz"},
		{Room, "Đakovo", "dakovo"},
		{Room, "Ølstue", "olstue"},
		{Room, "Ｂureau ﬁle m²", "bureau-file-m2"},
		{Room, "ℌall", "hall"},
		{Room, "Кухня", ""},
		{Device, "Lampe 東京 salon", "lampe-salon"},
		{Room, "Þvottahús með", "thvottahus-med"},
		{Room, "Işık Kapı", "isik-kapi"},
		{Room, "Ħamrun ŧ", "hamrun-t"},
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
		{Room, "Entrée", `"Entrée" isn't a room name: ` + Rule + `, such as "entree"`},
		{Client, "!!", `"!!" isn't a client name: ` + Rule},
		{Group, "", "a group needs a name"},
	} {
		err := CheckName(c.kind, c.name)
		if err == nil || err.Error() != c.want {
			t.Errorf("CheckName(%s, %q) = %v, want %s", c.kind, c.name, err, c.want)
		}
	}
}

// A log names an ID by its name, as a filter on it expects.
func TestAnIDLogsAsItsName(t *testing.T) {
	var out strings.Builder
	slog.New(slog.NewJSONHandler(&out, nil)).Info("Plugin came online", "plugin", ID{Name: "ewelink-lan"})
	if !strings.Contains(out.String(), `"plugin":"ewelink-lan"`) {
		t.Errorf("got %s", out.String())
	}
}

func TestValidVarName(t *testing.T) {
	for name, want := range map[string]bool{
		"TOSHIBA_USER": true, "A": true, "A1": true, "A__B": true, "A_": true,
		"": false, "toshiba_user": false, "_A": false, "1A": false, "A-B": false, "A.B": false, "A B": false,
		"Ä": false,
	} {
		if got := ValidVarName(name); got != want {
			t.Errorf("ValidVarName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestCheckVarNameSuggestsAName(t *testing.T) {
	if err := CheckVarName("TAHOMA_PIN"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"toshiba-user":       `"toshiba-user" isn't a variable name: ` + VarRule + `, such as "TOSHIBA_USER"`,
		"Cozytouch.Password": `"Cozytouch.Password" isn't a variable name: ` + VarRule + `, such as "COZYTOUCH_PASSWORD"`,
		"_token":             `"_token" isn't a variable name: ` + VarRule + `, such as "TOKEN"`,
		"2fa code":           `"2fa code" isn't a variable name: ` + VarRule + `, such as "VAR_2FA_CODE"`,
		"—":                  `"—" isn't a variable name: ` + VarRule,
		"":                   "a variable needs a name: " + VarRule,
	} {
		err := CheckVarName(name)
		if err == nil || err.Error() != want {
			t.Errorf("CheckVarName(%q) = %v, want %s", name, err, want)
		}
	}
}

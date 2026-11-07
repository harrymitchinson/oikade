package host

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/store"
)

func openAPIStore(t *testing.T) *store.Namespace {
	t.Helper()
	data, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	return data.Namespace(apiNamespace)
}

func TestTokensAreKeptOnlyAsHashes(t *testing.T) {
	values := openAPIStore(t)
	c, err := loadClients(values)
	if err != nil {
		t.Fatal(err)
	}
	token, err := c.add("laptop")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := values.Keys()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		raw, _, _ := values.Get(k)
		if bytes.Contains(raw, []byte(token)) || strings.Contains(k, token) {
			t.Errorf("%s holds the token", k)
		}
	}
	again, err := loadClients(values)
	if err != nil {
		t.Fatal(err)
	}
	name, revoked, ok := again.check(token)
	if !ok || name != "laptop" {
		t.Errorf("the token after a restart: %q, %v", name, ok)
	}
	if _, _, ok := again.check(token + "x"); ok {
		t.Error("a wrong token worked")
	}
	if err := again.revoke("laptop"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := again.check(token); ok {
		t.Error("a revoked token worked")
	}
	select {
	case <-revoked:
	default:
		t.Error("revoking didn't say so to the client's streams")
	}
	if last, err := loadClients(values); err != nil || len(last.list()) != 0 {
		t.Errorf("a revoked client came back: %v, %v", last.list(), err)
	}
}

func TestClientNamesAndDuplicates(t *testing.T) {
	c, err := loadClients(openAPIStore(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "Laptop", "a/b", "../x", strings.Repeat("a", 64)} {
		if _, err := c.add(bad); err == nil {
			t.Errorf("%q named a client", bad)
		}
	}
	if _, err := c.add("ci"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.add("ci"); err == nil {
		t.Error("two clients called ci")
	}
}

func TestLastUseIsWrittenAtMostOnceAMinute(t *testing.T) {
	values := openAPIStore(t)
	c, err := loadClients(values)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	token, _ := c.add("ci")
	c.check(token)
	now = now.Add(30 * time.Second)
	c.check(token)
	if got := c.list()[0].LastUsed; !got.Equal(now.Add(-30 * time.Second)) {
		t.Errorf("last use %v moved within a minute", got)
	}
	now = now.Add(time.Minute)
	c.check(token)
	reloaded, _ := loadClients(values)
	if got := reloaded.list()[0].LastUsed; !got.Equal(now) {
		t.Errorf("last use on disk is %v, want %v", got, now)
	}
}

func TestAPIIdentityIsMadeOnceAndKept(t *testing.T) {
	values := openAPIStore(t)
	_, pin, err := loadAPIIdentity(values)
	if err != nil {
		t.Fatal(err)
	}
	_, again, err := loadAPIIdentity(values)
	if err != nil || again != pin {
		t.Errorf("the pin changed from %s to %s: %v", pin, again, err)
	}
	if !strings.HasPrefix(pin, "sha256:") || len(pin) != len("sha256:")+64 {
		t.Errorf("pin %q", pin)
	}
	if err := values.Put(apiIdentityKey, []byte("{")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadAPIIdentity(values); err == nil {
		t.Error("an unreadable identity was replaced rather than refused")
	}
}

func TestAPIAddressDefaultsThePort(t *testing.T) {
	for in, want := range map[string]string{
		"pi.home":      "pi.home:8443",
		"pi.home:9000": "pi.home:9000",
		":8443":        ":8443",
		"::1":          "[::1]:8443",
		"[::1]":        "[::1]:8443",
		"192.168.1.5":  "192.168.1.5:8443",
	} {
		if got := APIAddress(in); got != want {
			t.Errorf("APIAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestALoginCodeWorksOnceAndNotForLong(t *testing.T) {
	c, err := loadClients(openAPIStore(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	code, _, err := c.issue("laptop")
	if err != nil {
		t.Fatal(err)
	}
	name, token, err := c.redeem(code)
	if err != nil || name != "laptop" {
		t.Fatalf("redeeming: %q, %v", name, err)
	}
	if got, _, ok := c.check(token); !ok || got != "laptop" {
		t.Errorf("the login's token: %q, %v", got, ok)
	}
	if _, _, err := c.redeem(code); err == nil {
		t.Error("a code worked twice")
	}

	code, _, _ = c.issue("phone")
	now = now.Add(codeLifetime)
	if _, _, err := c.redeem(code); err == nil {
		t.Error("an expired code worked")
	}

	// A wrong guess uses nothing up, and a new code for a name replaces the last.
	first, _, _ := c.issue("tablet")
	second, _, _ := c.issue("tablet")
	if _, _, err := c.redeem("nothing"); err == nil {
		t.Error("a made-up code worked")
	}
	if _, _, err := c.redeem(first); err == nil {
		t.Error("a replaced code worked")
	}
	if _, _, err := c.redeem(second); err != nil {
		t.Errorf("the newest code: %v", err)
	}
	if _, _, err := c.issue("laptop"); err == nil {
		t.Error("a code for a name a client has")
	}
}

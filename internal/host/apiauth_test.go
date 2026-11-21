package host

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"oikade.dev/internal/host/api"
	"oikade.dev/internal/store"
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
	if _, err := again.remove("laptop"); err != nil {
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
	for _, bad := range []string{"", "Laptop", "a/b", "../x", "my.laptop", "ci_2", "2nd", strings.Repeat("a", 64)} {
		if _, err := c.add(bad); err == nil {
			t.Errorf("%q named a client", bad)
		}
	}
	if _, err := c.add("My Laptop"); err == nil || !strings.Contains(err.Error(), `such as "my-laptop"`) {
		t.Errorf("err = %v, want a suggestion", err)
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

// A client waiting for its login is listed, with when its code runs out, until it does, and removing
// it cancels the code.
func TestAClientWaitingForItsLoginIsListedAndCanBeCancelled(t *testing.T) {
	c, err := loadClients(openAPIStore(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	if _, err := c.add("ci"); err != nil {
		t.Fatal(err)
	}
	code, expires, err := c.issue("laptop")
	if err != nil {
		t.Fatal(err)
	}
	// A name has a code or a token, never both, so a remove leaves nothing to log in with.
	if _, err := c.add("laptop"); err == nil || !strings.Contains(err.Error(), "waiting for its login") {
		t.Errorf("a token for a client waiting for its login: %v", err)
	}
	want := []api.Client{
		{Name: "ci", Ref: "client/ci", Created: now},
		{Name: "laptop", Ref: "client/laptop", Created: now, Expires: expires},
	}
	if got := c.list(); !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	cancelled, err := c.remove("laptop")
	if err != nil || !cancelled {
		t.Fatalf("cancelling laptop's login: %v, %v", cancelled, err)
	}
	if _, _, err := c.redeem(code); err == nil {
		t.Error("a cancelled code worked")
	}
	if got := c.list(); len(got) != 1 {
		t.Errorf("after the cancel: %+v", got)
	}
	if cancelled, err := c.remove("ci"); err != nil || cancelled {
		t.Errorf("removing ci: %v, %v", cancelled, err)
	}

	// A code that ran out is neither listed nor cancelled.
	if _, _, err := c.issue("phone"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(codeLifetime)
	if got := c.list(); len(got) != 0 {
		t.Errorf("an expired code is listed: %+v", got)
	}
	if _, err := c.remove("phone"); err == nil || !strings.Contains(err.Error(), "there's no client/phone") {
		t.Errorf("removing an expired code: %v", err)
	}
}

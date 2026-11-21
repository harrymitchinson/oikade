package host

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"oikade.dev/internal/host/api"
)

// away runs a command as a machine with no access to the host's data directory would: its own
// config, and no socket.
func away(t *testing.T, config string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(ctlBin, args...)
	cmd.Env = append(os.Environ(), append([]string{"XDG_CONFIG_HOME=" + config, "OIKADE_TOKEN="}, env...)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func TestLoginFromAnotherMachineThenEveryCommandOverTLS(t *testing.T) {
	t.Parallel()
	r := start(t, "--api-addr", "127.0.0.1:0")
	address, pin := r.network()
	var added struct{ Code, Pin, Address string }
	if err := json.Unmarshal(r.stdout(nil, "add", "client/laptop", "--json"), &added); err != nil {
		t.Fatal(err)
	}
	if added.Pin != pin || added.Address != address {
		t.Errorf("client add says %s at %s, and the host %s at %s", added.Pin, added.Address, pin, address)
	}
	laptop := t.TempDir()

	// The pin is checked before the code goes anywhere, so the code still works after.
	wrong := "sha256:" + strings.Repeat("0", 64)
	if out, err := away(t, laptop, nil, "login", address, "--pin", wrong, "--code", added.Code); err == nil {
		t.Errorf("logged in with the wrong pin: %s", out)
	}
	out, err := away(t, laptop, nil, "login", address, "--pin", pin, "--code", added.Code, "--context", "home")
	if err != nil {
		t.Fatalf("logging in: %v: %s", err, out)
	}
	if out, err := away(t, t.TempDir(), nil, "login", address, "--pin", pin, "--code", added.Code); err == nil {
		t.Errorf("a code worked twice: %s", out)
	}

	for _, args := range [][]string{
		{"status", "--json"},
		{"set", "device/siren/on", "on"},
		{"pause", "case/fan/on/fan"},
		{"resume", "case/fan/on/fan"},
		{"list", "devices", "--json"},
		{"list", "rules"},
		{"list", "plugins"},
		{"list", "clients"},
		{"backup"},
	} {
		if out, err := away(t, laptop, nil, args...); err != nil {
			t.Errorf("oikade %s over TLS: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	r.waitFor(func(l line) bool { return l.is("Received API request", "type", "backup", "client", "laptop") })

	// A deploy streams the binary, so the host needn't share a disk with the laptop.
	out, err = away(t, laptop, nil, "deploy", "--binary", variant(t, "renamed"))
	if err == nil || !strings.Contains(out, "--allow-safety") {
		t.Errorf("a deploy that drops a Safety rule went ahead over TLS: %v: %s", err, out)
	}
	out, err = away(t, laptop, nil, "deploy", "--binary", variant(t, "renamed"), "--allow-safety")
	if err != nil {
		t.Errorf("deploying over TLS: %v: %s", err, out)
	}
	if out, err := away(t, laptop, nil, "rollback", "--allow-safety"); err != nil {
		t.Errorf("rolling back over TLS: %v: %s", err, out)
	}
	if out, err := away(t, laptop, nil, "add", "client/more"); err == nil {
		t.Errorf("oikade client add: %s", out)
	}
	// Only the socket trusts a client, so a token can't make more of them.
	token, _ := r.addClient("thief")
	resp, err := api.Call(t.Context(), api.Remote{Address: address, Pin: pin, Token: token},
		api.Request{Type: api.TypeClientAdd, Client: "more"})
	if err == nil && resp.Error == "" {
		t.Errorf("client add over TLS: %+v", resp)
	}

	// CI has a token in a secret, and a login with no code that checks it.
	var ci struct{ Token string }
	if err := json.Unmarshal(r.stdout(nil, "add", "client/ci", "--token", "--json"), &ci); err != nil {
		t.Fatal(err)
	}
	runner := t.TempDir()
	withToken := []string{"OIKADE_TOKEN=" + ci.Token}
	if out, err := away(t, runner, withToken, "login", address, "--pin", pin); err != nil {
		t.Fatalf("CI's login: %v: %s", err, out)
	}
	if out, err := away(t, runner, withToken, "status"); err != nil {
		t.Errorf("CI's status: %v: %s", err, out)
	}
	if out, err := away(t, runner, nil, "status"); err == nil {
		t.Errorf("CI's context worked without its token: %s", out)
	}

	// Revoking from the laptop itself cuts it off.
	if out, err := away(t, laptop, nil, "remove", "client/laptop"); err != nil {
		t.Fatalf("revoking: %v: %s", err, out)
	}
	if out, err := away(t, laptop, nil, "status"); err == nil || !strings.Contains(out, "valid token") {
		t.Errorf("a revoked laptop: %v: %s", err, out)
	}
}

// A client waiting for its login is listed, and remove client/NAME cancels its code, so oikade login
// refuses it.
func TestACancelledLoginCodeIsRefused(t *testing.T) {
	t.Parallel()
	r := start(t, "--api-addr", "127.0.0.1:0")
	address, pin := r.network()
	var added struct{ Code string }
	if err := json.Unmarshal(r.stdout(nil, "add", "client/phone", "--json"), &added); err != nil {
		t.Fatal(err)
	}
	if listed := string(r.stdout(nil, "list", "clients")); !strings.Contains(listed, "waiting for its login") {
		t.Errorf("list clients:\n%s", listed)
	}
	if said := string(r.stdout(nil, "remove", "client/phone")); !strings.Contains(said, "Cancelled client/phone's login") {
		t.Errorf("remove said:\n%s", said)
	}
	out, err := away(t, t.TempDir(), nil, "login", address, "--pin", pin, "--code", added.Code)
	if err == nil || !strings.Contains(out, "that code doesn't work") {
		t.Errorf("logging in with a cancelled code: %v: %s", err, out)
	}
	if listed := string(r.stdout(nil, "list", "clients", "--json")); listed != "[]\n" {
		t.Errorf("list clients --json after the cancel: %s", listed)
	}
}

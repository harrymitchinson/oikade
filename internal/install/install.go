// Package install puts the host on a systemd machine, or upgrades the one there: oikade-host
// install. It copies the running binary to /usr/local/bin, makes the user oikade, writes the unit,
// starts it and waits until the host serves. An upgrade backs up the store first, keeps the old
// binary beside the new one, and puts it back if the new one doesn't serve (OIK-277).
package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
)

const (
	// BinPath is where the host's binary goes, and PreviousPath where an upgrade keeps the one before.
	BinPath      = "/usr/local/bin/oikade-host"
	PreviousPath = BinPath + ".previous"
	// UnitName is the systemd unit, and UnitPath its file. Changes of your own go in a drop-in,
	// from systemctl edit oikade, which install never touches.
	UnitName = "oikade"
	UnitPath = "/etc/systemd/system/" + UnitName + ".service"
	// User is who the host runs as.
	User = "oikade"
	// DefaultAPIAddr is where a new install serves the network API, and DefaultClient who its login
	// line is for.
	DefaultAPIAddr = ":" + api.DefaultPort
	DefaultClient  = "laptop"
	// DefaultWait is how long the host has to serve once it's started.
	DefaultWait = time.Minute
	// matterPort and mdnsPort are what a firewall must let in, besides the API.
	matterPort = "5540"
	mdnsPort   = "5353"
)

// Runner runs a command on this machine, and returns what it printed.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Caller sends a request to the host on this machine, through its socket.
type Caller func(ctx context.Context, req api.Request) (api.Response, error)

// Installer installs or upgrades the host.
type Installer struct {
	// Root is what the paths are under: / on a real machine.
	Root string
	// Self is the binary to install, and Version its version.
	Self    string
	Version string
	// Privileged is whether this runs as root.
	Privileged bool
	Run        Runner
	Call       Caller
	// Progress hears what install is doing as it does it.
	Progress io.Writer
	// Wait bounds how long the host has to serve, and Poll is how often it's asked.
	Wait, Poll time.Duration
}

// Options are what the command was given.
type Options struct {
	// APIAddr is the network API's address in the unit. A first install writes the unit with it, and
	// an upgrade only when APIAddrSet.
	APIAddr    string
	APIAddrSet bool
	// Client is who a login line is for. A first install always makes one, and an upgrade only when
	// ClientSet.
	Client    string
	ClientSet bool
}

// Result is what install did.
type Result struct {
	// Version is the version now installed.
	Version string
	// Upgraded is set when an install was there already, and From is its version.
	Upgraded bool
	From     string
	// Unchanged is an install of the binary that's there, with nothing to change.
	Unchanged bool
	// Previous is where the old binary is kept, after an upgrade that replaced it.
	Previous string
	// Backup is the snapshot of the store an upgrade took first, on the host.
	Backup string
	// Login is a new client's login, when one was made.
	Login *api.Response
	// Firewall is the firewall that's on, if install found one, and FirewallRule what lets the host
	// through it.
	Firewall     string
	FirewallRule string
}

// Install installs the host, or upgrades the one there.
func (in *Installer) Install(ctx context.Context, opts Options) (Result, error) {
	if !in.Privileged {
		return Result{}, errors.New("install writes to /usr/local/bin and /etc/systemd: run it with sudo")
	}
	if opts.APIAddr != "" {
		if err := checkAddr(opts.APIAddr); err != nil {
			return Result{}, err
		}
	}
	bin, unit := in.path(BinPath), in.path(UnitPath)
	oldUnit, err := os.ReadFile(unit)
	unitExists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	if unitExists && !runsHost(oldUnit) {
		return Result{}, fmt.Errorf("%s doesn't run %s, so install leaves it alone: "+
			"move it away and run install again, or upgrade by hand", UnitPath, BinPath)
	}
	_, err = os.Stat(bin)
	binExists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	if binExists && !unitExists {
		return Result{}, fmt.Errorf("%s is there but %s isn't, so the host may run from a unit install doesn't "+
			"know: upgrade it by hand, or remove %s and run install again", BinPath, UnitPath, BinPath)
	}
	if !binExists {
		return in.first(ctx, opts, unitExists)
	}
	return in.upgrade(ctx, opts, oldUnit)
}

// first installs the host where there's no binary.
func (in *Installer) first(ctx context.Context, opts Options, unitExists bool) (Result, error) {
	res := Result{Version: in.Version}
	in.say("Installing oikade-host %s to %s", in.Version, BinPath)
	if err := copyFile(in.Self, in.path(BinPath)); err != nil {
		return res, err
	}
	if _, err := in.Run(ctx, "id", "-u", User); err != nil {
		if _, err := in.Run(ctx, "useradd", "--system", "--home-dir", cli.DefaultDataDir,
			"--shell", "/usr/sbin/nologin", User); err != nil {
			return res, err
		}
		in.say("Created user %s", User)
	}
	if !unitExists || opts.APIAddrSet {
		if err := in.writeUnit(opts.APIAddr); err != nil {
			return res, err
		}
	}
	if err := in.start(ctx, true); err != nil {
		return res, err
	}
	if err := in.waitServing(ctx); err != nil {
		return res, fmt.Errorf("%w: journalctl -u %s says why", err, UnitName)
	}
	if opts.APIAddr != "" || opts.ClientSet {
		login, err := in.addClient(ctx, opts.Client)
		if err != nil {
			return res, err
		}
		res.Login = login
	}
	if opts.APIAddr != "" {
		res.Firewall, res.FirewallRule = in.firewall(ctx, opts.APIAddr)
	}
	return res, nil
}

// upgrade replaces the binary that's there with this one, and the unit only if it was given flags.
// The store is backed up first. If the new version doesn't serve, the old binary and unit come back.
func (in *Installer) upgrade(ctx context.Context, opts Options, oldUnit []byte) (Result, error) {
	bin := in.path(BinPath)
	res := Result{Version: in.Version, Upgraded: true, From: in.installedVersion(ctx, bin)}
	same, err := sameFile(in.Self, bin)
	if err != nil {
		return res, err
	}
	if same && !opts.APIAddrSet {
		in.say("oikade-host %s is installed at %s already", in.Version, BinPath)
		res.Unchanged = true
		if opts.ClientSet {
			res.Login, err = in.addClient(ctx, opts.Client)
		}
		return res, err
	}
	if same {
		in.say("Changing the unit of oikade-host %s", in.Version)
	} else {
		in.say("Upgrading oikade-host %s to %s", res.From, in.Version)
	}
	backup, err := in.call(ctx, api.Request{Type: api.TypeBackup})
	if err != nil {
		return res, fmt.Errorf("couldn't back up the store first, as the host isn't serving (%w): "+
			"sudo systemctl start %s, or upgrade by hand", err, UnitName)
	}
	res.Backup = backup.Backup
	in.say("Backed up the store to %s", res.Backup)
	if !same {
		if err := copyFile(bin, in.path(PreviousPath)); err != nil {
			return res, err
		}
		if err := copyFile(in.Self, bin); err != nil {
			return res, err
		}
		res.Previous = PreviousPath
	}
	if opts.APIAddrSet {
		if err := in.writeUnit(opts.APIAddr); err != nil {
			return res, errors.Join(err, in.putBack(ctx, !same, oldUnit))
		}
	}
	if err := in.start(ctx, opts.APIAddrSet); err != nil {
		return res, errors.Join(err, in.putBack(ctx, !same, oldUnit))
	}
	if err := in.waitServing(ctx); err != nil {
		if back := in.putBack(ctx, !same, oldUnit); back != nil {
			return res, fmt.Errorf("%w, and putting %s back failed too: %w: journalctl -u %s says why",
				err, res.From, back, UnitName)
		}
		if err := in.waitServing(ctx); err != nil {
			return res, fmt.Errorf("%s didn't serve, and nor does %s, which is back: journalctl -u %s says why",
				in.Version, res.From, UnitName)
		}
		return res, fmt.Errorf("%s didn't serve within %s, so %s is back: journalctl -u %s says why",
			in.Version, in.Wait, res.From, UnitName)
	}
	if opts.ClientSet {
		if res.Login, err = in.addClient(ctx, opts.Client); err != nil {
			return res, err
		}
	}
	return res, nil
}

// putBack restores the binary, if the upgrade replaced it, and the unit, if it had one, then
// restarts the host.
func (in *Installer) putBack(ctx context.Context, binary bool, oldUnit []byte) error {
	in.say("Putting the previous version back")
	if binary {
		if err := copyFile(in.path(PreviousPath), in.path(BinPath)); err != nil {
			return err
		}
	}
	if err := writeFile(in.path(UnitPath), oldUnit, 0o644); err != nil {
		return err
	}
	return in.start(ctx, true)
}

// start restarts the host, which starts it if it's stopped. A changed unit is reloaded first.
func (in *Installer) start(ctx context.Context, reload bool) error {
	if reload {
		if _, err := in.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
		if _, err := in.Run(ctx, "systemctl", "enable", UnitName); err != nil {
			return err
		}
	}
	in.say("Starting %s, and waiting for it to serve", UnitName)
	_, err := in.Run(ctx, "systemctl", "restart", UnitName)
	return err
}

// waitServing waits until the host answers on its socket. systemctl restart has stopped the old
// process by the time it returns, so whatever answers is the new one.
func (in *Installer) waitServing(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, in.Wait)
	defer cancel()
	for {
		if _, err := in.call(ctx, api.Request{Type: api.TypeInfo}); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s didn't serve within %s", UnitName, in.Wait)
		case <-time.After(in.Poll):
		}
	}
}

func (in *Installer) addClient(ctx context.Context, name string) (*api.Response, error) {
	resp, err := in.call(ctx, api.Request{Type: api.TypeClientAdd, Client: name})
	if err != nil {
		return nil, fmt.Errorf("couldn't add the client %s: %w", name, err)
	}
	return &resp, nil
}

// call sends a request, and makes the answer's error an error.
func (in *Installer) call(ctx context.Context, req api.Request) (api.Response, error) {
	resp, err := in.Call(ctx, req)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	return resp, err
}

// installedVersion asks the binary there. One too old to answer is still upgraded.
func (in *Installer) installedVersion(ctx context.Context, bin string) string {
	out, err := in.Run(ctx, bin, "version")
	if v := strings.TrimSpace(string(out)); err == nil && v != "" && !strings.ContainsAny(v, " \n") {
		return v
	}
	return "an unknown version"
}

// firewall says whether ufw is on, and what lets the host through it. It changes nothing.
func (in *Installer) firewall(ctx context.Context, apiAddr string) (string, string) {
	out, err := in.Run(ctx, "ufw", "status")
	if err != nil || !strings.Contains(string(out), "Status: active") {
		return "", ""
	}
	_, port, _ := net.SplitHostPort(api.Address(apiAddr))
	return "ufw", fmt.Sprintf("sudo ufw allow %s/udp && sudo ufw allow %s/udp && sudo ufw allow %s/tcp",
		matterPort, mdnsPort, port)
}

func (in *Installer) writeUnit(apiAddr string) error {
	if err := writeFile(in.path(UnitPath), []byte(Unit(apiAddr)), 0o644); err != nil {
		return err
	}
	if apiAddr == "" {
		in.say("Wrote %s, with the API only on the socket", UnitPath)
	} else {
		in.say("Wrote %s, with the API on %s", UnitPath, apiAddr)
	}
	return nil
}

func (in *Installer) say(format string, args ...any) {
	_, _ = fmt.Fprintf(in.Progress, format+"\n", args...)
}

func (in *Installer) path(p string) string {
	return filepath.Join(in.Root, p)
}

// Unit is the unit install writes. apiAddr empty serves the API only on the socket.
func Unit(apiAddr string) string {
	exec := BinPath
	if apiAddr != "" {
		exec += " --api-addr " + apiAddr
	}
	return `# Written by oikade-host install, which writes it again when it's given flags.
# Put changes of your own in a drop-in, with systemctl edit ` + UnitName + `, which it never touches.
[Unit]
Description=Oikade host
After=network-online.target
Wants=network-online.target

[Service]
User=` + User + `
ExecStart=` + exec + `
StateDirectory=oikade
StateDirectoryMode=0700
Restart=always
RestartSec=2
KillMode=mixed
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
`
}

// runsHost is whether a unit's ExecStart is the host's binary, so install may take it over.
func runsHost(unit []byte) bool {
	for line := range strings.SplitSeq(string(unit), "\n") {
		cmd, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart=")
		if !ok {
			continue
		}
		fields := strings.Fields(strings.TrimLeft(cmd, "-@:+!"))
		if len(fields) > 0 && fields[0] == BinPath {
			return true
		}
	}
	return false
}

// checkAddr refuses an address the unit couldn't carry as one word.
func checkAddr(addr string) error {
	if strings.ContainsAny(addr, " \t\r\n\"'\\%$;") {
		return fmt.Errorf("--api-addr %q isn't an address", addr)
	}
	if _, _, err := net.SplitHostPort(api.Address(addr)); err != nil {
		return fmt.Errorf("--api-addr %q isn't an address: %w", addr, err)
	}
	return nil
}

func sameFile(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(ai, bi), nil
}

// copyFile copies an executable, so that a crash leaves either the old file or the new one.
func copyFile(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return writeFile(to, data, 0o755)
}

// writeFile replaces a file by renaming a synced copy over it, and syncs the directory, so the new
// file is on disk before anything depends on it.
func writeFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// Package install puts the host on a systemd machine, or upgrades the one there: oikade-host
// install. It copies the running binary to /usr/local/bin, makes the user oikade, writes the unit,
// starts it and waits until the host serves. An upgrade backs up the store first, keeps the old
// binary beside the new one, and puts it back if the new one doesn't serve.
package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"oikade.dev/internal/cli"
	"oikade.dev/internal/host"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/store"
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
	// Client is who a login line is for. A first install makes one when the unit serves the network
	// API, and an upgrade only when ClientSet.
	Client    string
	ClientSet bool
}

// Result is what install did.
type Result struct {
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
		return in.first(ctx, opts, oldUnit)
	}
	return in.upgrade(ctx, opts, oldUnit)
}

// first installs the host where there's no binary.
func (in *Installer) first(ctx context.Context, opts Options, oldUnit []byte) (Result, error) {
	res := Result{Version: in.Version}
	in.say("Installing oikade-host %s to %s", in.Version, BinPath)
	if _, err := in.Run(ctx, "id", "-u", User); err != nil {
		if _, err := in.Run(ctx, "useradd", "--system", "--home-dir", cli.DefaultDataDir,
			"--shell", "/usr/sbin/nologin", User); err != nil {
			return res, err
		}
		in.say("Created user %s", User)
	}
	// The unit goes first, so an install that fails part way is one install finishes: a unit with
	// no binary is a first install, and a binary with no unit isn't.
	apiAddr := opts.APIAddr
	if oldUnit == nil || opts.APIAddrSet {
		if err := in.writeUnit(apiAddr); err != nil {
			return res, err
		}
	} else {
		apiAddr = unitAPIAddr(oldUnit)
	}
	if err := copyFile(in.Self, in.path(BinPath)); err != nil {
		return res, err
	}
	if err := in.start(ctx, true); err != nil {
		return res, err
	}
	if err := in.waitServing(ctx); err != nil {
		return res, fmt.Errorf("%w: journalctl -u %s says why", err, UnitName)
	}
	if apiAddr != "" || opts.ClientSet {
		login, err := in.addClient(ctx, opts.Client)
		if err != nil {
			return res, err
		}
		res.Login = login
	}
	if apiAddr != "" {
		res.Firewall, res.FirewallRule = in.firewall(ctx, apiAddr)
	}
	return res, nil
}

// upgrade replaces the binary that's there with this one, and the unit only if it was given flags.
// The store is backed up first. If the new version doesn't serve, the old binary and unit come
// back.
func (in *Installer) upgrade(ctx context.Context, opts Options, oldUnit []byte) (Result, error) {
	bin := in.path(BinPath)
	res := Result{Version: in.Version, Upgraded: true, From: in.installedVersion(ctx, bin)}
	// The same bytes are the same release, whichever file they're in: installing it again mustn't
	// put it over the previous version.
	same, err := sameContent(in.Self, bin)
	if err != nil {
		return res, err
	}
	// A unit written before the host kept your values in memory gains the directory for them. Flags
	// write the unit whole, with it.
	withRuntime, addRuntime := withRuntimeDir(oldUnit)
	addRuntime = addRuntime && !opts.APIAddrSet
	if same && !opts.APIAddrSet && !addRuntime {
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
	// The new binary is staged before the old one is kept, so Self may be the previous version itself.
	var staged string
	if !same {
		if staged, err = stage(in.Self, bin, 0o755); err != nil {
			return res, err
		}
		defer func() { _ = os.Remove(staged) }()
	}
	if err := in.backup(ctx, &res); err != nil {
		return res, err
	}
	// From here on, a cancel mustn't stop the old version coming back.
	undo := context.WithoutCancel(ctx)
	if !same {
		if err := copyFile(bin, in.path(PreviousPath)); err != nil {
			return res, err
		}
		if err := commit(staged, bin); err != nil {
			return res, err
		}
		res.Previous = PreviousPath
	}
	switch {
	case opts.APIAddrSet:
		if err := in.writeUnit(opts.APIAddr); err != nil {
			return res, errors.Join(err, in.putBack(undo, !same, oldUnit))
		}
	case addRuntime:
		if err := writeFile(in.path(UnitPath), withRuntime, 0o644); err != nil {
			return res, errors.Join(err, in.putBack(undo, !same, oldUnit))
		}
		in.say("Added %s to %s, for your values", RuntimeDir, UnitPath)
	}
	if err := in.start(ctx, opts.APIAddrSet || addRuntime); err != nil {
		return res, errors.Join(err, in.putBack(undo, !same, oldUnit))
	}
	if err := in.waitServing(ctx); err != nil {
		if back := in.putBack(undo, !same, oldUnit); back != nil {
			return res, fmt.Errorf("%w, and putting %s back failed too: %w: journalctl -u %s says why",
				err, res.From, back, UnitName)
		}
		if werr := in.waitServing(undo); werr != nil {
			return res, fmt.Errorf("%w, and nor does %s, which is back: journalctl -u %s says why",
				err, res.From, UnitName)
		}
		return res, fmt.Errorf("%w, so %s is back: journalctl -u %s says why", err, res.From, UnitName)
	}
	if opts.ClientSet {
		if res.Login, err = in.addClient(ctx, opts.Client); err != nil {
			return res, err
		}
	}
	return res, nil
}

// backup snapshots the store through the host. A host that isn't serving, such as one that crashes
// on start, is stopped so that the store can be copied whole. One with no store has nothing to
// lose.
func (in *Installer) backup(ctx context.Context, res *Result) error {
	backup, err := in.call(ctx, api.Request{Type: api.TypeBackup})
	if err == nil {
		res.Backup = backup.Backup
		in.say("Backed up the store to %s", res.Backup)
		return nil
	}
	dataDir := in.path(cli.DefaultDataDir)
	db := filepath.Join(dataDir, store.FileName)
	if _, serr := os.Stat(db); errors.Is(serr, os.ErrNotExist) {
		in.say("The host has no store yet, so there's nothing to back up")
		return nil
	}
	in.say("The host isn't serving (%v), so stopping %s to copy its store", err, UnitName)
	if _, err := in.Run(ctx, "systemctl", "stop", UnitName); err != nil {
		return err
	}
	dir := filepath.Join(dataDir, host.BackupsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	to := filepath.Join(dir, host.BackupName(time.Now()))
	if err := copyFile(db, to); err != nil {
		return err
	}
	// The host prunes its backups, and reads none it can't.
	if info, err := os.Stat(dataDir); err == nil {
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			if err := os.Chown(to, int(st.Uid), int(st.Gid)); err != nil {
				return err
			}
			if err := os.Chmod(to, 0o600); err != nil {
				return err
			}
		}
	}
	res.Backup = strings.TrimPrefix(to, filepath.Clean(in.Root))
	if !strings.HasPrefix(res.Backup, "/") {
		res.Backup = "/" + res.Backup
	}
	in.say("Backed up the store to %s", res.Backup)
	return nil
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
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, in.Wait)
	defer cancel()
	for {
		if _, err := in.call(ctx, api.Request{Type: api.TypeInfo}); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return fmt.Errorf("stopped waiting for %s %s to serve: %w", UnitName, in.Version, parent.Err())
			}
			return fmt.Errorf("%s %s didn't serve within %s", UnitName, in.Version, in.Wait)
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
` + strings.Join(runtimeDirLines, "") + `Restart=always
RestartSec=2
KillMode=mixed
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
`
}

// RuntimeDir is the directory systemd gives the host for your values, in memory.
const RuntimeDir = "/run/oikade"

// runtimeDirLines are the unit's lines that make RuntimeDir.
var runtimeDirLines = []string{"RuntimeDirectory=oikade\n", "RuntimeDirectoryMode=0700\n"}

// withRuntimeDir is a unit with the runtime directory added, if it has none: after its
// StateDirectoryMode, where Unit has it, or else its [Service] line. It's false for a unit that has
// one already, or no [Service] section to put it in.
func withRuntimeDir(unit []byte) ([]byte, bool) {
	lines := strings.SplitAfter(string(unit), "\n")
	at := -1
	inService := false
	for i, line := range lines {
		line = strings.TrimSpace(line)
		key, _, _ := strings.Cut(line, "=")
		switch {
		case strings.TrimSpace(key) == "RuntimeDirectory":
			return unit, false
		case strings.HasPrefix(line, "["):
			inService = line == "[Service]"
			if inService && at < 0 {
				at = i + 1
			}
		case inService && strings.TrimSpace(key) == "StateDirectoryMode":
			at = i + 1
		}
	}
	if at < 0 {
		return unit, false
	}
	if !strings.HasSuffix(lines[at-1], "\n") {
		lines[at-1] += "\n"
	}
	return []byte(strings.Join(slices.Concat(lines[:at], runtimeDirLines, lines[at:]), "")), true
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

// unitAPIAddr is the --api-addr a unit's ExecStart gives the host, or "" for none.
func unitAPIAddr(unit []byte) string {
	for line := range strings.SplitSeq(string(unit), "\n") {
		cmd, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart=")
		if !ok {
			continue
		}
		fields := strings.Fields(cmd)
		for i, f := range fields {
			if v, ok := strings.CutPrefix(f, "--api-addr="); ok {
				return v
			}
			if f == "--api-addr" && i+1 < len(fields) {
				return fields[i+1]
			}
		}
	}
	return ""
}

// sameContent is whether two files hold the same bytes.
func sameContent(a, b string) (bool, error) {
	ha, err := hash(a)
	if err != nil {
		return false, err
	}
	hb, err := hash(b)
	if err != nil {
		return false, err
	}
	return ha == hb, nil
}

func hash(path string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil {
		return sum, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// copyFile copies an executable, so that a crash leaves either the old file or the new one.
func copyFile(from, to string) error {
	tmp, err := stage(from, to, 0o755)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	return commit(tmp, to)
}

// writeFile replaces a file with data, so that a crash leaves either the old file or the new one.
func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := stageReader(bytes.NewReader(data), path, mode)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	return commit(tmp, path)
}

// stage copies from into a synced file beside to, for commit to rename over it.
func stage(from, to string, mode os.FileMode) (string, error) {
	f, err := os.Open(from)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return stageReader(f, to, mode)
}

func stageReader(r io.Reader, to string, mode os.FileMode) (string, error) {
	dir := filepath.Dir(to)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(to)+".*")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, r)
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
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// commit renames a staged file over to, and syncs the directory, so the new file is on disk before
// anything depends on it.
func commit(tmp, to string) error {
	if err := os.Rename(tmp, to); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

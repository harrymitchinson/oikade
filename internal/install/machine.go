// Package install puts a host on a machine with systemd: one process, one data directory, secrets as
// files, logs on stdout, and metrics and health over HTTP (OIK-143).
package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/harrymitchinson/oikade/internal/host"
	"github.com/harrymitchinson/oikade/internal/host/api"
)

const (
	// BinaryPath is on the PATH of a non-interactive SSH session, which oikade --host needs.
	BinaryPath = "/usr/local/bin/oikade"
	// previousSuffix is the binary an upgrade replaced, next to the new one.
	previousSuffix = ".previous"
	unitName       = "oikade.service"
	storeFile      = "oikade.db"
	unitPath       = "/etc/systemd/system/" + unitName
	// systemUser runs the host when nobody logged in to ask for it, such as root over SSH.
	systemUser = "oikade"
	// upWithin is how long a host has to answer after it starts.
	upWithin = 60 * time.Second
)

// Machine is a host under systemd, on the machine this runs on.
type Machine struct {
	// Binary is the oikade to install.
	Binary string
	// User runs the host. Empty means whoever ran sudo, or a system user if nobody did. oikade --host
	// needs its SSH user to be this one.
	User string
	// DataDir is the host's data directory.
	DataDir string
	// HostArgs are flags for oikade host.
	HostArgs []string
	// Out hears what's happening.
	Out io.Writer
}

// Unit renders the systemd unit. It isn't sandboxed: a deploy's binary is uploaded to /tmp, your code
// and plugins are yours to trust, and plugins may need hardware or files no sandbox can guess at.
func Unit(hostUser, dataDir string, hostArgs []string) (string, error) {
	command := []string{BinaryPath, "host", "--data-dir", dataDir}
	for _, arg := range append(slices.Clip(command), hostArgs...) {
		if strings.ContainsAny(arg, "\n\r") {
			return "", fmt.Errorf("%q has a line break, which a unit file can't hold", arg)
		}
	}
	quoted := make([]string, 0, len(command)+len(hostArgs))
	for _, arg := range append(command, hostArgs...) {
		quoted = append(quoted, quoteUnit(arg))
	}
	return `[Unit]
Description=Oikade host
Documentation=https://github.com/harrymitchinson/oikade
Wants=network-online.target
After=network-online.target

[Service]
User=` + hostUser + `
ExecStart=` + strings.Join(quoted, " ") + `
Restart=always
RestartSec=2
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
`, nil
}

// quoteUnit makes arg one word of an ExecStart line, where % and $ mean something too.
func quoteUnit(arg string) string {
	arg = strings.ReplaceAll(arg, "%", "%%")
	arg = strings.ReplaceAll(arg, "$", "$$")
	if arg != "" && !strings.ContainsAny(arg, " \t\"'\\;") {
		return arg
	}
	arg = strings.ReplaceAll(arg, `\`, `\\`)
	return `"` + strings.ReplaceAll(arg, `"`, `\"`) + `"`
}

// Install leaves the host running. It only does what's missing, so that a second run changes nothing.
func (m Machine) Install(ctx context.Context) error {
	if err := m.preflight(); err != nil {
		return err
	}
	var did []string
	account, created, err := m.account(ctx)
	if err != nil {
		return err
	}
	if created {
		did = append(did, "created user "+account.Username)
	}
	switch same, err := sameFile(m.Binary, BinaryPath); {
	case err != nil:
		return err
	case same:
	case fileExists(unitPath):
		return errors.New("a different oikade is already installed here: use oikade upgrade, which takes a backup first")
	default:
		if err := replaceBinary(m.Binary, BinaryPath); err != nil {
			return err
		}
		did = append(did, "installed "+BinaryPath)
	}
	made, err := m.dataDir(account)
	if err != nil {
		return err
	}
	if made {
		did = append(did, "created "+m.DataDir)
	}
	unit, err := Unit(account.Username, m.DataDir, m.HostArgs)
	if err != nil {
		return err
	}
	current, err := os.ReadFile(unitPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	changed := !bytes.Equal(current, []byte(unit))
	if changed {
		if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
			return err
		}
		if err := systemctl(ctx, "daemon-reload"); err != nil {
			return err
		}
		did = append(did, "wrote "+unitPath)
	}
	if !quiet(ctx, "is-enabled", unitName) {
		if err := systemctl(ctx, "enable", unitName); err != nil {
			return err
		}
		did = append(did, "enabled "+unitName)
	}
	switch {
	case !quiet(ctx, "is-active", unitName):
		if err := systemctl(ctx, "start", unitName); err != nil {
			return err
		}
		did = append(did, "started "+unitName)
	case changed:
		if err := systemctl(ctx, "restart", unitName); err != nil {
			return err
		}
		did = append(did, "restarted "+unitName)
	}
	if err := waitUp(ctx, m.DataDir); err != nil {
		return err
	}
	if len(did) == 0 {
		_, _ = fmt.Fprintln(m.Out, "Nothing to change: the host is installed and running")
		return nil
	}
	for _, step := range did {
		_, _ = fmt.Fprintln(m.Out, "-", step)
	}
	_, _ = fmt.Fprintln(m.Out, "The host is running. oikade matter shows its pairing code")
	if account.Username == systemUser {
		_, _ = fmt.Fprintln(m.Out, "It runs as "+systemUser+", which nobody can log in as, so oikade deploy --host won't work. "+
			"To deploy over SSH, install with --user and a user who can log in")
	}
	return nil
}

func (m Machine) preflight() error {
	if os.Geteuid() != 0 {
		return errors.New("this needs root: run it with sudo")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("this machine has no systemd")
	}
	return nil
}

// account finds who runs the host, and creates the system user if that's who it is.
func (m Machine) account(ctx context.Context) (*user.User, bool, error) {
	name := m.User
	if name == "" {
		name = os.Getenv("SUDO_USER")
	}
	if name == "" || name == "root" {
		name = systemUser
	}
	found, err := user.Lookup(name)
	if err == nil {
		return found, false, nil
	}
	if name != systemUser {
		return nil, false, fmt.Errorf("finding user %s: %w", name, err)
	}
	out, err := exec.CommandContext(ctx, "useradd", "--system", "--no-create-home",
		"--home-dir", m.DataDir, "--shell", "/usr/sbin/nologin", name).CombinedOutput()
	if err != nil {
		return nil, false, fmt.Errorf("creating user %s: %w: %s", name, err, out)
	}
	found, err = user.Lookup(name)
	return found, true, err
}

// dataDir makes the directory, or gives what's in it to the host's user if it's somebody else's, as
// a restored one is. A host that can't open its own store can't say why from here. Parents it has to
// make stay readable, so that the host's user can get through them.
func (m Machine) dataDir(account *user.User) (bool, error) {
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return false, err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(m.DataDir)
	if err == nil {
		if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) == uid {
			return false, nil
		}
		return false, filepath.WalkDir(m.DataDir, func(path string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Lchown(path, uid, gid)
		})
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(m.DataDir), 0o755); err != nil {
		return false, err
	}
	if err := os.Mkdir(m.DataDir, 0o700); err != nil {
		return false, err
	}
	return true, os.Chown(m.DataDir, uid, gid)
}

// Upgrade swaps the binary and restarts the host, having taken a backup. Restarting takes Matter down
// with it, so it says for how long. A new binary that won't come up gives way to the old one.
func (m Machine) Upgrade(ctx context.Context) error {
	if err := m.preflight(); err != nil {
		return err
	}
	if !fileExists(unitPath) {
		return errors.New("no host is installed here: use oikade install")
	}
	switch same, err := sameFile(m.Binary, BinaryPath); {
	case err != nil:
		return err
	case same:
		_, _ = fmt.Fprintln(m.Out, "Already on this version")
		return nil
	}
	backup, err := m.backup(ctx)
	if err != nil {
		return fmt.Errorf("taking a backup, so not upgrading: %w", err)
	}
	if backup == "" {
		_, _ = fmt.Fprintln(m.Out, "The host has no store yet, so there's nothing to back up")
	} else {
		_, _ = fmt.Fprintln(m.Out, "Backed up the store to", backup)
	}
	previous := BinaryPath + previousSuffix
	if err := copyFile(BinaryPath, previous); err != nil {
		return err
	}
	// From the swap on, stopping halfway leaves the file and the process disagreeing, and a rerun
	// can't see that. So it finishes, which upWithin bounds, even if whoever asked goes away.
	ctx = context.WithoutCancel(ctx)
	signal.Ignore(syscall.SIGHUP)
	if err := replaceBinary(m.Binary, BinaryPath); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(m.Out, "Restarting the host. Matter, your automations and your plugins stop until it's back, "+
		"which takes seconds")
	began := time.Now()
	err = systemctl(ctx, "restart", unitName)
	if err == nil {
		err = waitUp(ctx, m.DataDir)
	}
	if err == nil {
		_, _ = fmt.Fprintf(m.Out, "Upgraded. The host was down for %s\n", time.Since(began).Round(100*time.Millisecond))
		return nil
	}
	_, _ = fmt.Fprintln(m.Out, "The new version didn't come up, so going back:", err)
	if err := replaceBinary(previous, BinaryPath); err != nil {
		return fmt.Errorf("going back: %w. The old binary is %s, and the store's backup %s", err, previous, backup)
	}
	if err := systemctl(ctx, "restart", unitName); err != nil {
		return fmt.Errorf("going back: %w. The store's backup is %s", err, backup)
	}
	if err := waitUp(ctx, m.DataDir); err != nil {
		return fmt.Errorf("the old version didn't come up either: %w. The store's backup is %s, "+
			"which goes back as %s while the host is stopped", err, backup, filepath.Join(m.DataDir, storeFile))
	}
	return errors.New("the upgrade failed, and the old version is running again")
}

// backup asks a running host for a snapshot. One that isn't running may be between crashes, so it's
// stopped for good first, which lets go of the file. It may never have made one: that's nothing to
// back up, and it returns "".
func (m Machine) backup(ctx context.Context) (string, error) {
	if quiet(ctx, "is-active", unitName) {
		resp, err := api.Call(ctx, api.Local{DataDir: m.DataDir}, api.Request{Type: api.TypeBackup})
		if err != nil {
			return "", err
		}
		if resp.Error != "" {
			return "", errors.New(resp.Error)
		}
		return resp.Backup, nil
	}
	if err := systemctl(ctx, "stop", unitName); err != nil {
		return "", err
	}
	from := filepath.Join(m.DataDir, storeFile)
	if !fileExists(from) {
		return "", nil
	}
	dir := filepath.Join(m.DataDir, host.BackupsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, host.BackupName(time.Now()))
	if err := copyFile(from, path+".new"); err != nil {
		return "", err
	}
	if err := os.Rename(path+".new", path); err != nil {
		return "", err
	}
	// This runs as root, and the host has to write here later.
	info, err := os.Stat(m.DataDir)
	if err != nil {
		return "", err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if err := errors.Join(os.Chown(dir, int(st.Uid), int(st.Gid)), os.Chown(path, int(st.Uid), int(st.Gid))); err != nil {
			return "", err
		}
	}
	return path, host.PruneBackups(dir)
}

// waitUp waits for the host to answer and for its Matter bridge to serve, as that's what a restart
// costs the Home app. A host with Matter off answers with an error, and one that's broken says why.
func waitUp(ctx context.Context, dataDir string) error {
	limit, cancel := context.WithTimeout(ctx, upWithin)
	defer cancel()
	var last error
	for {
		resp, err := api.Call(limit, api.Local{DataDir: dataDir}, api.Request{Type: api.TypeMatter})
		if err == nil && (resp.Matter == nil || resp.Matter.Serving || resp.Matter.Problem != "") {
			return nil
		}
		if err != nil {
			last = err
		}
		select {
		case <-limit.Done():
			return fmt.Errorf("the host didn't answer within %s: %w. journalctl -u %s says why", upWithin, last, unitName)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func systemctl(ctx context.Context, args ...string) error {
	if out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return nil
}

// quiet is a systemctl question, which answers with its exit code.
func quiet(ctx context.Context, args ...string) bool {
	return exec.CommandContext(ctx, "systemctl", append(slices.Clip(args), "--quiet")...).Run() == nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func sameFile(a, b string) (bool, error) {
	if !fileExists(b) {
		return false, nil
	}
	x, err := digest(a)
	if err != nil {
		return false, err
	}
	y, err := digest(b)
	if err != nil {
		return false, err
	}
	return x == y, nil
}

func digest(path string) ([sha256.Size]byte, error) {
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
	h.Sum(sum[:0])
	return sum, nil
}

// replaceBinary swaps a file in with one rename, as a running binary's file can't be written to.
func replaceBinary(from, to string) error {
	next := to + ".new"
	if err := copyFile(from, next); err != nil {
		return err
	}
	if err := os.Chmod(next, 0o755); err != nil {
		return err
	}
	return os.Rename(next, to)
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	return errors.Join(err, out.Close())
}

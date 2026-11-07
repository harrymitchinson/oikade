package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"

	"golang.org/x/term"

	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/install"
)

// currentVersion is this binary's: what a release's -ldflags said, or what go install recorded.
func currentVersion() string {
	if version != devVersion {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

// publishedVersion is the version go can fetch of what this binary is, or main if it's a local build.
func publishedVersion() string {
	if v := currentVersion(); strings.HasPrefix(v, "v") && !strings.Contains(v, "+") {
		return v
	}
	return "main"
}

// errContextOverSSH is install and upgrade given a context, which they can't use: they go over SSH.
var errContextOverSSH = errors.New("install and upgrade go over SSH, with --host, not a context")

func runInstall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	t := addTarget(fs)
	binary := fs.String("binary", "", "The oikade to install, built for that machine. "+
		"Defaults to this one, or a build of this version.")
	account := fs.String("user", "", "Who runs the host. Defaults to who logged in. "+
		"oikade --host needs to log in as them.")
	printOnly := fs.Bool("print", false, "Print the systemd unit, and install nothing.")
	_ = fs.Parse(args)
	if *t.context != "" {
		return errContextOverSSH
	}
	hostArgs := fs.Args()
	if slices.ContainsFunc(hostArgs, func(a string) bool {
		return strings.HasPrefix(strings.TrimLeft(a, "-"), "data-dir")
	}) {
		return errors.New("pass --data-dir before the --, to oikade install")
	}
	if *printOnly {
		unit, err := install.Unit(cmp.Or(*account, "USER"), *t.dataDir, hostArgs)
		if err != nil {
			return err
		}
		fmt.Print(unit)
		return nil
	}
	return onMachine(ctx, *t.host, *t.dataDir, *binary, *account, "install", hostArgs,
		func(m install.Machine) error { return m.Install(ctx) })
}

// onMachine runs install or upgrade on the machine it's for. Over SSH, it copies an oikade there and
// has that one do it, as root, so there's one way of doing it.
func onMachine(ctx context.Context, host, dataDir, binary, account, command string, hostArgs []string,
	here func(install.Machine) error) error {
	if host == "" {
		if binary == "" {
			self, err := os.Executable()
			if err != nil {
				return err
			}
			binary = self
		}
		return here(install.Machine{Binary: binary, User: account, DataDir: dataDir, HostArgs: hostArgs, Out: os.Stdout})
	}
	ssh := api.SSH{Host: host, DataDir: dataDir}
	if binary == "" {
		goos, goarch, err := machinePlatform(ctx, ssh)
		if err != nil {
			return err
		}
		path, done, err := binaryFor(ctx, goos, goarch)
		if err != nil {
			return err
		}
		defer done()
		binary = path
	}
	remote, uploaded, err := ssh.Upload(ctx, binary)
	if err != nil {
		return err
	}
	defer uploaded()
	words := []string{"$sudo", api.Quote(remote), command, "--binary", api.Quote(remote), "--data-dir", api.Quote(dataDir)}
	if account != "" {
		words = append(words, "--user", api.Quote(account))
	}
	if len(hostArgs) > 0 {
		words = append(words, "--")
		for _, arg := range hostArgs {
			words = append(words, api.Quote(arg))
		}
	}
	line := fmt.Sprintf(`sudo=sudo; [ "$(id -u)" = 0 ] && sudo=; chmod +x %s && %s`,
		api.Quote(remote), strings.Join(words, " "))
	if err := ssh.Attached(ctx, line); err != nil {
		return fmt.Errorf("oikade %s on %s: %w", command, host, err)
	}
	return nil
}

// machinePlatform asks a machine what it is, in Go's words.
func machinePlatform(ctx context.Context, ssh api.SSH) (string, string, error) {
	out, err := ssh.Output(ctx, "uname -sm")
	if err != nil {
		return "", "", fmt.Errorf("reaching %s: %w", ssh.Host, err)
	}
	return parsePlatform(out)
}

func parsePlatform(uname string) (string, string, error) {
	fields := strings.Fields(uname)
	if len(fields) != 2 || fields[0] != "Linux" {
		return "", "", fmt.Errorf("oikade install needs Linux, and this is %q", strings.TrimSpace(uname))
	}
	switch fields[1] {
	case "aarch64", "arm64":
		return "linux", "arm64", nil
	case "x86_64", "amd64":
		return "linux", "amd64", nil
	}
	return "", "", fmt.Errorf("oikade has no build for %s: it runs on 64-bit Arm and x86", fields[1])
}

// binaryFor finds an oikade of this version for a platform: this one if it fits, or else a build.
func binaryFor(ctx context.Context, goos, goarch string) (string, func(), error) {
	if goos == runtime.GOOS && goarch == runtime.GOARCH {
		self, err := os.Executable()
		return self, func() {}, err
	}
	v := currentVersion()
	if v == devVersion || strings.Contains(v, "+dirty") {
		return "", nil, fmt.Errorf("this oikade is a local build, so there's no version to build for %s/%s: "+
			"pass --binary, built with GOOS=%[1]s GOARCH=%[2]s CGO_ENABLED=0", goos, goarch)
	}
	dir, err := os.MkdirTemp("", "oikade-binary")
	if err != nil {
		return "", nil, err
	}
	done := func() { _ = exec.Command("chmod", "-R", "u+w", dir).Run(); _ = os.RemoveAll(dir) }
	cache, err := exec.CommandContext(ctx, "go", "env", "GOMODCACHE").Output()
	if err != nil {
		done()
		return "", nil, fmt.Errorf("finding your Go module cache, as a build for %s/%s needs Go: %w", goos, goarch, err)
	}
	// go install puts a cross-compiled binary under GOPATH and refuses GOBIN, so it gets its own GOPATH.
	cmd := exec.CommandContext(ctx, "go", "install", "-ldflags", "-X main.version="+v, frameworkModule+"/cmd/oikade@"+v)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0", "GOBIN=", "GOPATH="+dir,
		"GOMODCACHE="+strings.TrimSpace(string(cache)))
	if err := cmd.Run(); err != nil {
		done()
		return "", nil, fmt.Errorf("building oikade %s for %s/%s: %w", v, goos, goarch, err)
	}
	return filepath.Join(dir, "bin", goos+"_"+goarch, "oikade"), done, nil
}

func runUpgrade(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	t := addTarget(fs)
	binary := fs.String("binary", "", "The oikade to upgrade to, built for that machine. "+
		"Defaults to this one, or a build of this version.")
	_ = fs.Parse(args)
	if *t.context != "" {
		return errContextOverSSH
	}
	return onMachine(ctx, *t.host, *t.dataDir, *binary, "", "upgrade", nil,
		func(m install.Machine) error { return m.Upgrade(ctx) })
}

func runBackup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	t := addTarget(fs)
	_ = fs.Parse(args)
	resp, err := api.Call(ctx, t.transport(), api.Request{Type: api.TypeBackup})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	fmt.Println(resp.Backup)
	return nil
}

func runSecret(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("secret", flag.ExitOnError)
	t := addTarget(fs)
	asJSON := fs.Bool("json", false, "Print JSON, whose fields stay stable for scripts.")
	words := parse(fs, args)
	req := api.Request{}
	switch {
	case len(words) == 1 && words[0] == "list":
		req.Type = api.TypeSecrets
	case len(words) == 2 && words[0] == "set":
		value, err := readSecret(os.Stdin)
		if err != nil {
			return err
		}
		req = api.Request{Type: api.TypeSecretSet, Secret: words[1], SecretValue: value}
	case len(words) == 2 && words[0] == "remove":
		req = api.Request{Type: api.TypeSecretRemove, Secret: words[1]}
	default:
		return errors.New("usage: oikade secret list | set NAME | remove NAME")
	}
	resp, err := api.Call(ctx, t.transport(), req)
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	if req.Type != api.TypeSecrets {
		if !*asJSON {
			fmt.Println("Done. Your process and plugins read it when they next ask, so one that reads it once needs a restart")
			return nil
		}
		return printJSON(struct{}{})
	}
	if *asJSON {
		return printJSON(resp.Secrets)
	}
	for _, s := range resp.Secrets {
		fmt.Println(s.Name)
	}
	return nil
}

// readSecret takes a value from a pipe, or asks for it without echoing. Never from an argument,
// which would leave it in ps and the shell's history.
func readSecret(in *os.File) (string, error) {
	var value []byte
	var err error
	if term.IsTerminal(int(in.Fd())) {
		fmt.Fprint(os.Stderr, "Value: ")
		value, err = term.ReadPassword(int(in.Fd()))
		fmt.Fprintln(os.Stderr)
	} else {
		value, err = io.ReadAll(in)
	}
	if err != nil {
		return "", err
	}
	text := strings.TrimSuffix(strings.TrimSuffix(string(value), "\n"), "\r")
	if text == "" {
		return "", errors.New("the value is empty")
	}
	return text, nil
}

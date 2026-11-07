package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"

	"github.com/harrymitchinson/oikade/internal/host"
	"github.com/harrymitchinson/oikade/internal/host/api"
)

// tokenEnv overrides a context's token, as CI has one in a secret.
const tokenEnv = "OIKADE_TOKEN"

// clientConfig is the oikade command's own settings: the hosts it has logged in to. It holds tokens,
// so only its owner may read it.
type clientConfig struct {
	// Current is the context commands use when they're given none.
	Current  string                 `json:"current,omitempty"`
	Contexts map[string]hostContext `json:"contexts,omitempty"`
}

// hostContext is one host's network API, as a login found it.
type hostContext struct {
	Address string `json:"address"`
	Pin     string `json:"pin"`
	// Token is empty for a context whose token comes from OIKADE_TOKEN.
	Token string `json:"token,omitempty"`
}

func configPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "oikade", "config.json"), nil
}

// loadConfig reads the config, which is empty if there's none. One that others can read is refused,
// as it may hold tokens that are as good as a login on the host.
func loadConfig() (clientConfig, error) {
	var cfg clientConfig
	path, err := configPath()
	if err != nil {
		return cfg, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return cfg, fmt.Errorf("%s can be read by others, and holds tokens: run chmod 600 %s", path, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("reading %s: %w", path, err)
	}
	return cfg, nil
}

// saveConfig writes the config whole, with mode 0600, so a crash leaves the old one or the new.
func saveConfig(cfg clientConfig) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// remote is a context's transport. One saved with no token, as CI's is, takes OIKADE_TOKEN, so a
// token meant for one host never goes to another.
func remote(c hostContext) api.Remote {
	token := c.Token
	if token == "" {
		token = os.Getenv(tokenEnv)
	}
	return api.Remote{Address: c.Address, Pin: c.Pin, Token: token}
}

// failing is a transport that can't be made, which says why when it's used.
type failing struct{ err error }

func (f failing) Dial(context.Context) (net.Conn, error) { return nil, f.err }

func (f failing) Upload(context.Context, string) (string, func(), error) { return "", nil, f.err }

// runLogin trusts a host's network API by its pin, and saves it as the current context.
func runLogin(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("login", flag.ExitOnError)
	pin := flags.String("pin", "", "The host's pin, from oikade client add.")
	code := flags.String("code", "", "The code from oikade client add. Without one, the token comes from "+tokenEnv+".")
	name := flags.String("context", "", "What to call this host. Defaults to its name.")
	words := parse(flags, args)
	if len(words) != 1 || *pin == "" {
		return errors.New("usage: oikade login ADDRESS --pin PIN [--code CODE] [--context NAME]")
	}
	address := host.APIAddress(words[0])
	if *name == "" {
		*name, _, _ = net.SplitHostPort(address)
	}
	// Read first: a code works once, so nothing after it may fail for want of a config.
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	saved := hostContext{Address: address, Pin: *pin}
	who := ""
	if *code != "" {
		resp, err := api.Call(ctx, api.Remote{Address: address, Pin: *pin}, api.Request{Type: api.TypeLogin, Code: *code})
		if err != nil {
			return err
		}
		if resp.Error != "" {
			return errors.New(resp.Error)
		}
		saved.Token, who = resp.Token, resp.Client
	} else {
		if os.Getenv(tokenEnv) == "" {
			return fmt.Errorf("oikade login needs --code, or a token in %s", tokenEnv)
		}
		// Checked now, rather than at the first command.
		resp, err := api.Call(ctx, remote(saved), api.Request{Type: api.TypeInfo})
		if err != nil {
			return err
		}
		if resp.Error != "" {
			return errors.New(resp.Error)
		}
	}
	if cfg.Contexts == nil {
		cfg.Contexts = map[string]hostContext{}
	}
	cfg.Contexts[*name], cfg.Current = saved, *name
	if err := saveConfig(cfg); err != nil {
		if saved.Token == "" {
			return err
		}
		// The code is used up, so the token is all there is: say it rather than lose it.
		return fmt.Errorf("saving the login: %w. Your token for %s is %s: keep it in %s, "+
			"then run oikade login %s --pin %s", err, address, saved.Token, tokenEnv, address, *pin)
	}
	switch who {
	case "":
		fmt.Printf("Logged in to %s, with the token in %s. Commands use it as %s.\n", address, tokenEnv, *name)
	default:
		fmt.Printf("Logged in to %s as %s. Commands use it as %s, unless you give --data-dir.\n", address, who, *name)
	}
	return nil
}

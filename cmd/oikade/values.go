package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/envfile"
	"github.com/harrymitchinson/oikade/internal/host/wire"
)

// dotEnv is the file of your values, beside your home's go.mod.
const dotEnv = ".env"

// values are what a deploy sends the host as your values (OIK-365), and where they came from.
type values struct {
	set map[string]string
	// file is the file read, or to add a value to, as you'd name it.
	file string
	// fromFile and fromEnv count the values the file gave and those the environment did.
	fromFile, fromEnv int
}

// readValues reads your values, as collectValues does, and refuses any of used that nothing gives.
func readValues(module, envFile string, used []string, lookup func(string) (string, bool)) (values, error) {
	v, missing, err := collectValues(module, envFile, used, lookup)
	switch {
	case err != nil:
		return values{}, err
	case len(missing) == 1:
		return values{}, cli.Errorf("%s isn't set", missing[0]).
			WithHint("add it to %s, or set it in the environment", v.file)
	case len(missing) > 1:
		return values{}, cli.Errorf("%s aren't set", andList(missing)).
			WithHint("add them to %s, or set them in the environment", v.file)
	}
	return v, nil
}

// collectValues reads your values: envFile if it's given, which must be there, or the .env beside
// module's go.mod, if there is one. The environment wins, for a name in the file or one in used,
// the names the home's code uses, and missing are those of used that nothing gives a value.
func collectValues(module, envFile string, used []string, lookup func(string) (string, bool)) (
	v values, missing []string, err error,
) {
	path, given := envFile, envFile != ""
	if !given {
		root, _ := goModDir(module)
		path = filepath.Join(root, dotEnv)
	}
	v = values{set: map[string]string{}, file: shown(path)}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !given:
		// A home that needs no values needs no file.
	case errors.Is(err, fs.ErrNotExist):
		return values{}, nil, cli.Errorf("there's no %s", v.file).
			WithHint("--env-file names a file of your values, a NAME=VALUE a line")
	case err != nil:
		return values{}, nil, cli.Wrap(err, "couldn't read %s", v.file)
	default:
		if v.set, err = envfile.Parse(string(raw)); err != nil {
			return values{}, nil, cli.Errorf("%s:%s", v.file, err)
		}
	}
	for name := range v.set {
		if value, ok := lookup(name); ok {
			v.set[name] = value
			v.fromEnv++
		} else {
			v.fromFile++
		}
	}
	for _, name := range used {
		if _, ok := v.set[name]; ok {
			continue
		}
		if value, ok := lookup(name); ok {
			v.set[name] = value
			v.fromEnv++
			continue
		}
		missing = append(missing, name)
	}
	return v, missing, nil
}

// warnMissing says which values your code uses that nothing here gives, and what that means: then,
// such as "a deploy from here would stop".
func (v values) warnMissing(p *cli.Printer, missing []string, then string) {
	switch len(missing) {
	case 0:
		return
	case 1:
		p.Warn("%s isn't set, so %s", missing[0], then)
		p.Hint("add it to %s, or set it in the environment", v.file)
	default:
		p.Warn("%s aren't set, so %s", andList(missing), then)
		p.Hint("add them to %s, or set them in the environment", v.file)
	}
}

// valuesFile writes the file's values where a home run here reads them, as the host's file is, and
// returns the environment that names it to the process, with the environment winning for any name, as
// it does for a deploy's values, and how to remove it. So the home has what a deploy would send it
// with one run, though only its run says which names its code uses.
func valuesFile(set map[string]string) (env []string, remove func(), err error) {
	if set == nil {
		set = map[string]string{}
	}
	raw, err := json.Marshal(set)
	if err != nil {
		return nil, nil, err
	}
	// CreateTemp makes it 0600, so no one else can read it.
	f, err := os.CreateTemp("", "oikade-values-*.json")
	if err != nil {
		return nil, nil, cli.Wrap(err, "couldn't write your values for your home").
			WithHint("set TMPDIR to a directory you can write to")
	}
	remove = func() { _ = os.Remove(f.Name()) }
	_, err = f.Write(raw)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		remove()
		return nil, nil, cli.Wrap(err, "couldn't write your values for your home").
			WithHint("set TMPDIR to a directory you can write to")
	}
	return []string{wire.VarsEnv + "=" + f.Name(), wire.VarsFromEnvironment + "=1"}, remove, nil
}

// homeValues runs your home's check, the binary at path, with your values as a deploy would send
// them, and returns what it says and the values, with the names its code uses that nothing gives.
func homeValues(ctx context.Context, module, path, envFile string, lookup func(string) (string, bool)) (
	wire.Manifest, values, []string, error,
) {
	given, _, err := collectValues(module, envFile, nil, lookup)
	if err != nil {
		return wire.Manifest{}, values{}, nil, err
	}
	m, err := checkWith(ctx, path, given.set)
	if err != nil {
		return wire.Manifest{}, given, nil, err
	}
	all, missing, err := collectValues(module, envFile, m.Vars, lookup)
	if err != nil {
		return wire.Manifest{}, values{}, nil, err
	}
	return m, all, missing, nil
}

// checkWith runs the check of the home at path with your values, as the host runs a deploy's.
func checkWith(ctx context.Context, path string, set map[string]string) (wire.Manifest, error) {
	env, remove, err := valuesFile(set)
	if err != nil {
		return wire.Manifest{}, err
	}
	defer remove()
	return readManifest(ctx, path, env)
}

// sent says how many values went, and where from, for the line after a deploy: ", with 6 values
// from .env". It's empty for none.
func (v values) sent() string {
	n := v.fromFile + v.fromEnv
	if n == 0 {
		return ""
	}
	noun := "values"
	if n == 1 {
		noun = "value"
	}
	from := v.file
	switch {
	case v.fromFile == 0:
		from = "the environment"
	case v.fromEnv > 0:
		from += " and the environment"
	}
	return fmt.Sprintf(", with %d %s from %s", n, noun, from)
}

// shown is a path as you'd type it: relative to here if it's inside, or as it is.
func shown(path string) string {
	wd, err := os.Getwd()
	if err != nil || !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(wd, path)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rel
	}
	return path
}

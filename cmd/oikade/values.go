package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/envfile"
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

// readValues reads your values: envFile if it's given, which must be there, or the .env beside
// module's go.mod, if there is one. The environment wins, for a name in the file or one in used,
// the names the home's code uses, which are all refused unless something gives them a value.
func readValues(module, envFile string, used []string, lookup func(string) (string, bool)) (values, error) {
	path, given := envFile, envFile != ""
	if !given {
		root, _ := goModDir(module)
		path = filepath.Join(root, dotEnv)
	}
	v := values{set: map[string]string{}, file: shown(path)}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !given:
		// A home that needs no values needs no file.
	case errors.Is(err, fs.ErrNotExist):
		return values{}, cli.Errorf("there's no %s", v.file).
			WithHint("--env-file names a file of your values, a NAME=VALUE a line")
	case err != nil:
		return values{}, cli.Wrap(err, "couldn't read %s", v.file)
	default:
		if v.set, err = envfile.Parse(string(raw)); err != nil {
			return values{}, cli.Errorf("%s:%s", v.file, err)
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
	var missing []string
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
	if len(missing) == 1 {
		return values{}, cli.Errorf("%s isn't set", missing[0]).
			WithHint("add it to %s, or set it in the environment", v.file)
	}
	if len(missing) > 1 {
		return values{}, cli.Errorf("%s aren't set", andList(missing)).
			WithHint("add them to %s, or set them in the environment", v.file)
	}
	return v, nil
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

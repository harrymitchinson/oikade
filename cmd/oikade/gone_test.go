package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// gone are the commands, flags and names that were replaced, each with what replaced it.
var gone = []struct {
	pattern *regexp.Regexp
	now     string
}{
	{regexp.MustCompile(`oikade devices\b`), "oikade list devices, or oikade forget"},
	{regexp.MustCompile(`oikade secrets?\b`), "a value in .env, which oikade.Var reads"},
	{regexp.MustCompile(`oikade client\b`), "oikade list clients, or oikade remove client/NAME"},
	{regexp.MustCompile(`oikade-host client\b`), "oikade-host add client/NAME"},
	{regexp.MustCompile(`oikade matter\b`), "oikade status matter, or oikade pair matter"},
	{regexp.MustCompile(`\b(devices remove|secret set|secret remove|client revoke|client add|matter open)\b`),
		"forget, a value in .env, remove or pair matter"},
	{regexp.MustCompile(`oikade (set|remove) secret/|oikade list secrets?\b|secret/NAME`),
		"a value in .env, which oikade.Var reads"},
	{regexp.MustCompile(`\b(oikade|plugin)\.(New)?Secret\b|ErrSecretNotSet|unsetSecrets`),
		"oikade.Var, and a plain string in a plugin's config"},
	{regexp.MustCompile(`--secrets-dir\b`), "a value in .env, or a file in the data directory's secrets"},
	{regexp.MustCompile(`--allow-safety-removal\b`), "--allow-safety"},
	{regexp.MustCompile(`oikade init\b[^\n]*--module\b[^-]`), "oikade init --module-path"},
}

// ghCommand is the text before a command of GitHub's gh: the word gh, then a space.
var ghCommand = regexp.MustCompile(`(^|\W)gh $`)

// No doc and no message names a command that's gone: every file in the repo, the skills agents read
// included, but the plan, test data, tests, which try the old forms on purpose, handoffs and the
// CHANGELOG, which are history, and what builds leave.
func TestNothingNamesACommandThatsGone(t *testing.T) {
	root := filepath.Join("..", "..")
	// From the root, as a package could be called plan or bin.
	skipDirs := []string{".git", "plan", "bin", "dist", ".claude/worktrees", ".claude/handoffs"}
	texts := []string{".go", ".md", ".json", ".yml", ".yaml", ".sh", ".tmpl", ".txt"}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if slices.Contains(skipDirs, filepath.ToSlash(rel)) || d.Name() == "testdata" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !slices.Contains(texts, filepath.Ext(path)) || strings.HasSuffix(path, "_test.go") ||
			d.Name() == "CHANGELOG.md" {
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // the repo's own files
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, g := range gone {
				for _, at := range g.pattern.FindAllStringIndex(line, -1) {
					// GitHub's gh has a secret set of its own, which the docs on deploying from CI run.
					if ghCommand.MatchString(line[:at[0]]) {
						continue
					}
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s:%d names %q, which is gone: %s", rel, i+1, strings.TrimSpace(line[at[0]:at[1]]), g.now)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

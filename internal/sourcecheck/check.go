// Package sourcecheck runs the checks of a home that only its source can show.
package sourcecheck

import (
	"errors"
	"fmt"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/packages"

	"github.com/harrymitchinson/oikade/internal/chaincheck"
	"github.com/harrymitchinson/oikade/internal/flowcheck"
)

// Check runs the analyzers over the module in dir, and returns what it found as "file:line: message".
// A package that doesn't compile is an error. env is the go command's environment; nil means this
// process's.
func Check(dir string, env []string) ([]string, error) {
	cfg := &packages.Config{Mode: packages.LoadAllSyntax, Dir: dir, Env: env}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("loading the module: %w", err)
	}
	var errs []error
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			errs = append(errs, e)
		}
	})
	if len(errs) > 0 {
		return nil, fmt.Errorf("loading the module: %w", errors.Join(errs...))
	}
	graph, err := checker.Analyze([]*analysis.Analyzer{flowcheck.Analyzer, chaincheck.Analyzer}, pkgs, nil)
	if err != nil {
		return nil, err
	}
	var found []string
	for _, act := range graph.Roots {
		if act.Err != nil {
			return nil, act.Err
		}
		for _, d := range act.Diagnostics {
			found = append(found, fmt.Sprintf("%s: %s", act.Package.Fset.Position(d.Pos), d.Message))
		}
	}
	return found, nil
}

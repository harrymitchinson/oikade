// Package chaincheck rejects a chained method on a framework type whose result isn't used. Those
// methods return a new value and leave the one they're called on alone, so the setting is lost, and
// nothing else would say so.
package chaincheck

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/types/typeutil"
)

const frameworkPath = "github.com/harrymitchinson/oikade/framework"

// Analyzer reports a call of a framework type's chained method that discards its result.
var Analyzer = &analysis.Analyzer{
	Name: "chaincheck",
	Doc:  "reject a framework rule's chained method whose result isn't used, which loses the setting",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch s := n.(type) {
			case *ast.ExprStmt:
				if call, ok := ast.Unparen(s.X).(*ast.CallExpr); ok {
					check(pass, call)
				}
			case *ast.DeferStmt:
				check(pass, s.Call)
			case *ast.GoStmt:
				check(pass, s.Call)
			}
			return true
		})
	}
	return nil, nil
}

// check reports call, whose result nothing uses, if it's a chained method. It points at the method,
// as a chain usually spans several lines.
func check(pass *analysis.Pass, call *ast.CallExpr) {
	fn, _ := typeutil.Callee(pass.TypesInfo, call).(*types.Func)
	if fn == nil {
		return
	}
	named := chained(fn)
	if named == nil {
		return
	}
	pos := call.Pos()
	if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
		pos = sel.Sel.Pos()
	}
	pass.Reportf(pos, "the result of %s isn't used, so it does nothing: %s's methods return a new value",
		fn.Name(), named.Obj().Name())
}

// chained returns fn's receiver type if fn is a method of a framework type that returns that type.
func chained(fn *types.Func) *types.Named {
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil || sig.Results().Len() != 1 {
		return nil
	}
	named, ok := types.Unalias(sig.Recv().Type()).(*types.Named)
	if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != frameworkPath {
		return nil
	}
	if !types.Identical(sig.Recv().Type(), sig.Results().At(0).Type()) {
		return nil
	}
	return named
}

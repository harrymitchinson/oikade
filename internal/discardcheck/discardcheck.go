// Package discardcheck rejects a call that builds an oikade value and throws it away. Everything
// in package oikade that returns one of its own types is a constructor or a chained method: it makes
// a value and does nothing else, and the value only acts once it's passed on, as a Set to r.Set or a
// condition to a case. Left as a statement, it's lost, and nothing else would say so.
package discardcheck

import (
	"fmt"
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/types/typeutil"
)

const oikadePath = "oikade.dev"

// Analyzer reports a call of an oikade function or method that discards the oikade value it
// returns.
var Analyzer = &analysis.Analyzer{
	Name: "discardcheck",
	Doc:  "reject an oikade value, such as a Set or a rule, that's built and thrown away",
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

// check reports call, whose result nothing uses, if it returns an oikade value. It points at the
// function's name, as a chain usually spans several lines.
func check(pass *analysis.Pass, call *ast.CallExpr) {
	fn, _ := typeutil.Callee(pass.TypesInfo, call).(*types.Func)
	if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != oikadePath {
		return
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Results().Len() != 1 {
		return
	}
	result := oikadeType(sig.Results().At(0).Type())
	if result == nil {
		return
	}
	pos := call.Pos()
	if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
		pos = sel.Sel.Pos()
	}
	pass.Reportf(pos, "the result of %s isn't used, so it does nothing: %s", fn.Name(), why(sig, result))
}

// exempt are the types added to their home as they're built, so a chain of one is a statement.
var exempt = map[string]bool{"Decision": true, "Flow": true, "Plugin": true, "Declaration": true}

// oikadeType returns t if it's a type package oikade declares, an instance of one, or a pointer to
// one, and isn't exempt.
func oikadeType(t types.Type) *types.Named {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		t = types.Unalias(p.Elem())
	}
	named, ok := t.(*types.Named)
	if !ok {
		return nil
	}
	obj := named.Origin().Obj()
	if obj.Pkg() == nil || obj.Pkg().Path() != oikadePath || exempt[obj.Name()] {
		return nil
	}
	return named
}

// why says what would have used a result of type result, from a function of type sig.
func why(sig *types.Signature, result *types.Named) string {
	name := result.Origin().Obj().Name()
	if sig.Recv() != nil && types.Identical(sig.Recv().Type(), result) {
		return fmt.Sprintf("%s's methods return a new value", name)
	}
	switch name {
	case "Set":
		return "a Set does nothing until r.Set takes it"
	case "Home":
		return "a Home does nothing until its Run runs it"
	case "Condition", "Hours", "Dates":
		return "a condition does nothing until something takes it"
	}
	return fmt.Sprintf("a %s does nothing until something takes it", name)
}

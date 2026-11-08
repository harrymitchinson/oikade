// Package flowcheck rejects code that breaks a flow's replay. After a crash a flow runs again from
// the top, and its steps return what they returned the first time, so it has to reach the same
// steps in the same order. The clock, goroutines and map order don't: each run sees a different
// time, schedule and order.
//
// A flow here is any function taking a *framework.FlowRun. A function it calls without passing that
// along isn't checked, which also means a helper can read the clock. What call passes to Do is
// exempt, as that's where outside calls belong and its result is recorded.
package flowcheck

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/types/typeutil"
)

const (
	frameworkPath = "github.com/harrymitchinson/oikade/framework"
	runType       = "FlowRun"

	useNow   = "r.Now()"
	useSleep = "r.Sleep"
)

// clockCalls are the time package's functions that depend on when they run, and what to use instead.
var clockCalls = map[string]string{
	"Now":       useNow,
	"Since":     useNow,
	"Until":     useNow,
	"Sleep":     useSleep,
	"After":     useSleep,
	"AfterFunc": useSleep,
	"NewTimer":  useSleep,
	"NewTicker": useSleep,
	"Tick":      useSleep,
}

// mapIterators are the maps package's functions that yield in a map's order. slices.Sorted over one
// is the way to sort the keys, and isn't a range over it.
var mapIterators = map[string]bool{"All": true, "Keys": true, "Values": true}

// Analyzer reports the clock and goroutines inside flows.
var Analyzer = &analysis.Analyzer{
	Name: "flowcheck",
	Doc:  "reject the clock and goroutines inside flows, which break their replay after a crash",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch fn := n.(type) {
			case *ast.FuncDecl:
				if fn.Body != nil && takesRun(pass, fn.Type) {
					checkBody(pass, fn.Body)
					return false
				}
			case *ast.FuncLit:
				if takesRun(pass, fn.Type) {
					checkBody(pass, fn.Body)
					return false
				}
			}
			return true
		})
	}
	return nil, nil
}

func takesRun(pass *analysis.Pass, ft *ast.FuncType) bool {
	if ft.Params == nil {
		return false
	}
	for _, p := range ft.Params.List {
		if isRun(pass.TypesInfo.TypeOf(p.Type)) {
			return true
		}
	}
	return false
}

func isRun(t types.Type) bool {
	ptr, ok := types.Unalias(t).(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := types.Unalias(ptr.Elem()).(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Name() == runType && obj.Pkg() != nil && obj.Pkg().Path() == frameworkPath
}

func checkBody(pass *analysis.Pass, body *ast.BlockStmt) {
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.GoStmt:
			pass.Reportf(s.Pos(), "a goroutine inside a flow breaks its replay after a crash: use steps in order")
		case *ast.RangeStmt:
			if rangesOverMap(pass, s.X) {
				pass.Reportf(s.Pos(), "ranging over a map inside a flow varies between runs, so it breaks replay: sort the keys")
			}
		case *ast.CallExpr:
			if isDo(pass, s) {
				// Its name and result still count as the flow's. Its call doesn't.
				for _, arg := range s.Args[:len(s.Args)-1] {
					checkBody(pass, &ast.BlockStmt{List: []ast.Stmt{&ast.ExprStmt{X: arg}}})
				}
				return false
			}
		case *ast.SelectorExpr:
			// Not only calls: a flow could keep time.Now in a variable.
			if name, ok := clockFunc(pass, s); ok {
				pass.Reportf(s.Pos(), "time.%s inside a flow varies between runs, so it breaks replay: use %s",
					name, clockCalls[name])
			}
		}
		return true
	})
}

// rangesOverMap reports whether what a range statement ranges over gives a map's order, which
// slices.Collect keeps.
func rangesOverMap(pass *analysis.Pass, x ast.Expr) bool {
	if _, ok := pass.TypesInfo.TypeOf(x).Underlying().(*types.Map); ok {
		return true
	}
	fn, call := callee(pass, x)
	if fn != nil && fn.Pkg() != nil && fn.Pkg().Path() == "slices" && fn.Name() == "Collect" && len(call.Args) == 1 {
		fn, _ = callee(pass, call.Args[0])
	}
	return fn != nil && fn.Pkg() != nil && fn.Pkg().Path() == "maps" && mapIterators[fn.Name()]
}

// callee returns the function x calls, or nil if x isn't a call of one.
func callee(pass *analysis.Pass, x ast.Expr) (*types.Func, *ast.CallExpr) {
	call, ok := ast.Unparen(x).(*ast.CallExpr)
	if !ok {
		return nil, nil
	}
	fn, _ := typeutil.Callee(pass.TypesInfo, call).(*types.Func)
	return fn, call
}

// isDo reports whether a call is FlowRun.Do, whose last argument is the outside call.
func isDo(pass *analysis.Pass, call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Do" || len(call.Args) == 0 {
		return false
	}
	return isRun(pass.TypesInfo.TypeOf(sel.X))
}

func clockFunc(pass *analysis.Pass, sel *ast.SelectorExpr) (string, bool) {
	fn, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "time" {
		return "", false
	}
	if sig, ok := fn.Type().(*types.Signature); !ok || sig.Recv() != nil {
		return "", false
	}
	_, known := clockCalls[fn.Name()]
	return fn.Name(), known
}

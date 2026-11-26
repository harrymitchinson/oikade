package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"

	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/host/wire"
)

// declarationsInSource reads the device declarations a home's source makes, for when it doesn't
// build: as when it already names a handle oikade types hasn't written. Only a chain from
// .Device("name") with literal arguments counts, which is how a declaration is written.
func declarationsInSource(module string) ([]wire.DeviceDeclaration, error) {
	var out []wire.DeviceDeclaration
	err := filepath.WalkDir(module, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && path != module && skipped(d.Name()):
			return filepath.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil // The compiler has said what's wrong with it.
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if hardware, ok := deviceHardwareOf(call); ok {
				out = append(out, wire.DeviceDeclaration{Hardware: hardware})
				return false
			}
			if decl, ok := declarationOf(call); ok {
				out = append(out, decl)
				return false
			}
			return true
		})
		return nil
	})
	return out, err
}

// skipped are directories with no home code in them.
func skipped(dir string) bool {
	return strings.HasPrefix(dir, ".") || dir == "vendor" || dir == "testdata"
}

// deviceHardwareOf reads the hardware of an oikade.Device[T] call, which is all oikade types needs of it.
func deviceHardwareOf(call *ast.CallExpr) (string, bool) {
	var fun ast.Expr
	switch f := call.Fun.(type) {
	case *ast.IndexExpr:
		fun = f.X
	case *ast.IndexListExpr:
		fun = f.X
	default:
		return "", false
	}
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Device" || len(call.Args) == 0 {
		return "", false
	}
	args := literals(call.Args[:1])
	return strings.Join(args, ""), len(args) == 1 && args[0] != ""
}

// declarationOf reads a chain of Device, Hardware, Room and Is calls, outermost first.
func declarationOf(call *ast.CallExpr) (wire.DeviceDeclaration, bool) {
	var d wire.DeviceDeclaration
	for {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return d, false
		}
		args := literals(call.Args)
		switch {
		case sel.Sel.Name == "Device" && len(args) == 1:
			d.Name = args[0]
			return d, d.Name != ""
		case sel.Sel.Name == "Hardware" && len(args) == 2:
			d.Plugin, d.ID = args[0], args[1]
		case sel.Sel.Name == "Room" && len(args) == 1:
			d.Room = &args[0]
		case sel.Sel.Name == "Is" && len(call.Args) == 1:
			lit, ok := call.Args[0].(*ast.CompositeLit)
			if !ok {
				return d, false
			}
			t, ok := lit.Type.(*ast.SelectorExpr)
			if !ok {
				return d, false
			}
			d.DeviceType = devicetype.Type(t.Sel.Name) // oikade's device types are named as Matter's are.
		default:
			return d, false
		}
		if call, ok = sel.X.(*ast.CallExpr); !ok {
			return d, false
		}
	}
}

// literals are a call's string arguments, if every one is a literal.
func literals(args []ast.Expr) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		lit, ok := a.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return nil
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return nil
		}
		out = append(out, s)
	}
	return out
}

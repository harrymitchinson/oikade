// Package apicheck fails a public package whose exported API mentions an internal one, which users
// of it couldn't name. It reads the source, so it needs no build.
package apicheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Check returns one line for each mention of an internal package in the exported API of the
// package in dir, ignoring its tests.
func Check(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var found []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		found = append(found, checkFile(fset, file)...)
	}
	slices.Sort(found)
	return found, nil
}

func checkFile(fset *token.FileSet, file *ast.File) []string {
	internal := internalImports(file)
	var found []string
	skip := map[*ast.Field]bool{}
	mentions := func(where string, n ast.Node) {
		if n == nil {
			return
		}
		ast.Inspect(n, func(n ast.Node) bool {
			switch t := n.(type) {
			case *ast.Field:
				return !skip[t]
			case *ast.StructType:
				skipUnexported(t.Fields, skip)
			case *ast.InterfaceType:
				skipUnexported(t.Methods, skip)
			case *ast.SelectorExpr:
				if pkg, ok := t.X.(*ast.Ident); ok && internal[pkg.Name] != "" {
					found = append(found, fmt.Sprintf("%s: %s mentions %s", fset.Position(t.Pos()), where, internal[pkg.Name]))
				}
			}
			return true
		})
	}
	for _, d := range file.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if !d.Name.IsExported() || unexportedReceiver(d) {
				continue
			}
			mentions(d.Name.Name, d.Type)
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					if s.Name.IsExported() {
						mentions(s.Name.Name, s.Type)
					}
				case *ast.ValueSpec:
					if slices.ContainsFunc(s.Names, func(i *ast.Ident) bool { return i.IsExported() }) {
						mentions(s.Names[0].Name, s.Type)
						for _, v := range s.Values {
							if _, isFunc := v.(*ast.FuncLit); !isFunc {
								mentions(s.Names[0].Name, v)
							}
						}
					}
				}
			}
		}
	}
	return found
}

// skipUnexported marks the fields or methods with no exported name, which aren't API. A parameter's
// name says nothing about that, so only a struct's or an interface's fields are marked.
func skipUnexported(fields *ast.FieldList, skip map[*ast.Field]bool) {
	for _, f := range fields.List {
		if len(f.Names) > 0 && !slices.ContainsFunc(f.Names, func(i *ast.Ident) bool { return i.IsExported() }) {
			skip[f] = true
		}
	}
}

func unexportedReceiver(d *ast.FuncDecl) bool {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return false
	}
	t := d.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if index, ok := t.(*ast.IndexExpr); ok {
		t = index.X
	}
	id, ok := t.(*ast.Ident)
	return ok && !id.IsExported()
}

// internalImports maps the names a file uses for internal packages to their paths.
func internalImports(file *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || !isInternal(path) {
			continue
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[name] = path
	}
	return out
}

func isInternal(path string) bool {
	return slices.Contains(strings.Split(path, "/"), "internal")
}

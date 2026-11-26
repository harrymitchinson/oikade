package main

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/ident"
)

// newDevice is a device oikade types adds to your devices file.
type newDevice struct {
	// name is what its Go name is made from: its app's name, or the name Home.Device declares it by.
	name string
	key  string
	room string
	typ  devicetype.Type
	// more are its capabilities beyond its type's.
	more []string
}

// added is what addDevices added.
type added struct {
	// names are the devices' Go names, by key.
	names map[string]string
	rooms []string
	// clashed are the devices whose names were taken, so they end with a hash of their hardware.
	clashed []string
}

// oikadePackage is the name a file imports oikade.dev by, unless it says another.
const oikadePackage = "oikade"

const devicesHeader = `// Your home's rooms and devices. oikade types adds each device you pair that isn't here yet, and
// never changes a line that is: rename, move and retype them here.
`

// addDevices adds a line to file for each device, and an oikade.Room for each of their rooms the
// package has none for, after what's there, which it never changes. file is your devices file, nil
// for a new one in package pkg, and others are the package's other files, whose names it doesn't
// take.
func addDevices(file []byte, pkg string, others [][]byte, devices []newDevice) ([]byte, added, error) {
	fset := token.NewFileSet()
	var f *ast.File
	q, imported := oikadePackage, false
	if file != nil {
		var err error
		if f, err = parser.ParseFile(fset, "", file, parser.SkipObjectResolution); err != nil {
			return nil, added{}, fmt.Errorf("it isn't Go: %w", err)
		}
		pkg = f.Name.Name
		q, imported = importName(f)
	}
	p := scanPackage(fset, pkg, append([][]byte{file}, others...))
	// What the package names already, whether or not its home builds.
	devices = slices.DeleteFunc(slices.Clone(devices), func(d newDevice) bool { return p.named[d.key] })
	out := added{names: map[string]string{}}
	roomLines, err := p.addRooms(q, devices, &out)
	if err != nil {
		return nil, added{}, err
	}
	slices.SortFunc(devices, func(a, b newDevice) int {
		return cmp.Or(strings.Compare(ident.GoName(ident.Device, a.name), ident.GoName(ident.Device, b.name)),
			strings.Compare(a.key, b.key))
	})
	var deviceLines []string
	for _, d := range devices {
		name, err := p.unique(ident.GoName(ident.Device, d.name), "hardware/"+d.key, &out)
		if err != nil {
			return nil, added{}, err
		}
		out.names[d.key] = name
		deviceLines = append(deviceLines, deviceLine(q, name, d, p.rooms[d.room]))
	}
	lines, err := formatVars(roomLines, deviceLines)
	if err != nil {
		return nil, added{}, err
	}
	if f == nil {
		return newFile(pkg, lines), out, nil
	}
	return appendLines(fset, f, file, imported, lines), out, nil
}

// pkgNames are what a package has: the names it declares, its rooms' names in Go by room, and the
// hardware its oikade.Device calls name.
type pkgNames struct {
	taken, named map[string]bool
	rooms        map[string]string
}

// scanPackage reads files, those of them in package pkg, which may be nil.
func scanPackage(fset *token.FileSet, pkg string, files [][]byte) pkgNames {
	p := pkgNames{taken: map[string]bool{}, named: map[string]bool{}, rooms: map[string]string{}}
	for _, raw := range files {
		if raw == nil {
			continue
		}
		f, err := parser.ParseFile(fset, "", raw, parser.SkipObjectResolution)
		if err != nil || f.Name.Name != pkg {
			continue
		}
		declaredNames(f, p.taken)
		roomsIn(f, p.rooms)
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if key, ok := deviceHardwareOf(call); ok {
					p.named[key] = true
				}
			}
			return true
		})
	}
	return p
}

// unique is name, or, if it's taken, name and the first 8 characters of ref's SHA-256, which it
// takes.
func (p pkgNames) unique(name, ref string, out *added) (string, error) {
	if p.taken[name] {
		sum := sha256.Sum256([]byte(ref))
		suffixed := name + hex.EncodeToString(sum[:])[:8]
		if p.taken[suffixed] {
			return "", fmt.Errorf("%s and %s are both taken in Go", name, suffixed)
		}
		name = suffixed
		out.clashed = append(out.clashed, name)
	}
	p.taken[name] = true
	return name, nil
}

// addRooms names each of devices' rooms the package has none for, and gives their lines.
func (p pkgNames) addRooms(q string, devices []newDevice, out *added) ([]string, error) {
	var rooms []string
	for _, d := range devices {
		if _, ok := p.rooms[d.room]; d.room != "" && !ok && !slices.Contains(rooms, d.room) {
			rooms = append(rooms, d.room)
		}
	}
	slices.Sort(rooms)
	lines := make([]string, 0, len(rooms))
	for _, room := range rooms {
		name, err := p.unique(ident.GoName(ident.Room, room), ident.NewRef(ident.Room, room).String(), out)
		if err != nil {
			return nil, err
		}
		p.rooms[room] = name
		out.rooms = append(out.rooms, name)
		lines = append(lines, fmt.Sprintf("%s = %s.Room(%q)", name, q, room))
	}
	return lines, nil
}

// deviceLine declares d as name, in the room whose Go name is room, if it has one.
func deviceLine(q, name string, d newDevice, room string) string {
	typ := q + "." + string(d.typ)
	if len(d.more) > 0 {
		fields := make([]string, 0, 1+len(d.more))
		fields = append(fields, typ)
		for _, c := range d.more {
			spec, _ := capability.Lookup(c)
			fields = append(fields, fmt.Sprintf("%s %s.%s", spec.Go, q, goKind(spec.Kind)))
		}
		typ = "struct {\n" + strings.Join(fields, "\n") + "\n}"
	}
	args := strconv.Quote(d.key)
	if room != "" {
		args += ", " + room
	}
	return fmt.Sprintf("%s = %s.Device[%s](%s)", name, q, typ, args)
}

// newFile is a devices file in package pkg with lines.
func newFile(pkg, lines string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	if lines != "" {
		b.WriteString("import \"oikade.dev\"\n\n")
	}
	b.WriteString(devicesHeader)
	if lines != "" {
		b.WriteString("\n" + lines)
	}
	return b.Bytes()
}

// appendLines is file, which is f, with lines after it, and oikade.dev imported if it isn't.
func appendLines(fset *token.FileSet, f *ast.File, file []byte, imported bool, lines string) []byte {
	if lines == "" {
		return file
	}
	result := slices.Clone(file)
	if !imported {
		at := fset.Position(f.Name.End()).Offset
		for _, decl := range f.Decls {
			if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.IMPORT {
				at = fset.Position(gen.End()).Offset
			}
		}
		result = slices.Concat(result[:at], []byte("\n\nimport \"oikade.dev\""), result[at:])
	}
	if !bytes.HasSuffix(result, []byte("\n")) {
		result = append(result, '\n')
	}
	return append(append(result, '\n'), lines...)
}

// formatVars is var declarations of each list of lines, as gofmt has them.
func formatVars(lists ...[]string) (string, error) {
	if !slices.ContainsFunc(lists, func(l []string) bool { return len(l) > 0 }) {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("package p\n\n")
	for _, lines := range lists {
		switch len(lines) {
		case 0:
		case 1:
			b.WriteString("var " + lines[0] + "\n\n")
		default:
			b.WriteString("var (\n" + strings.Join(lines, "\n") + "\n)\n\n")
		}
	}
	src, err := format.Source([]byte(b.String()))
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(string(src), "package p\n\n"), nil
}

// declaredNames adds the names f declares at package level to taken.
func declaredNames(f *ast.File, taken map[string]bool) {
	for _, decl := range f.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			if decl.Recv == nil {
				taken[decl.Name.Name] = true
			}
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch spec := spec.(type) {
				case *ast.ValueSpec:
					for _, n := range spec.Names {
						taken[n.Name] = true
					}
				case *ast.TypeSpec:
					taken[spec.Name.Name] = true
				}
			}
		}
	}
}

// roomsIn adds f's rooms at package level, var X = oikade.Room("name"), to rooms, by name.
func roomsIn(f *ast.File, rooms map[string]string) {
	q, ok := importName(f)
	if !ok {
		return
	}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			v, ok := spec.(*ast.ValueSpec)
			if !ok || len(v.Names) != len(v.Values) {
				continue
			}
			for i, value := range v.Values {
				if room, ok := roomCall(value, q); ok {
					if _, seen := rooms[room]; !seen {
						rooms[room] = v.Names[i].Name
					}
				}
			}
		}
	}
}

// roomCall reads q.Room("name").
func roomCall(e ast.Expr, q string) (string, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Room" {
		return "", false
	}
	if x, ok := sel.X.(*ast.Ident); !ok || x.Name != q {
		return "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	room, err := strconv.Unquote(lit.Value)
	return room, err == nil
}

// importName is the name f imports oikade.dev by, or false if it doesn't.
func importName(f *ast.File) (string, bool) {
	for _, imp := range f.Imports {
		if path, _ := strconv.Unquote(imp.Path.Value); path == oikadeModule {
			if imp.Name != nil {
				return imp.Name.Name, true
			}
			return oikadePackage, true
		}
	}
	return oikadePackage, false
}

// goKind is the Go type of a capability of kind k, in package oikade.
func goKind(k capability.Kind) string {
	return map[capability.Kind]string{
		capability.KindBool: "Bool", capability.KindNumber: "Number", capability.KindName: "Choice",
	}[k]
}

// generatedByOldTypes says file is one an older oikade types wrote whole, which it replaces.
func generatedByOldTypes(file []byte) bool {
	return bytes.HasPrefix(file, []byte("// Code generated by oikade types. DO NOT EDIT."))
}

// packageFiles are the Go files beside path, but path itself.
func packageFiles(path string) ([][]byte, error) {
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*.go"))
	if err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(path)
	var out [][]byte
	for _, m := range matches {
		if a, _ := filepath.Abs(m); a == abs {
			continue
		}
		raw, err := os.ReadFile(m)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

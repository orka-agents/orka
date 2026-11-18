package patchverification

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"strconv"
)

func maskGoCredentialReferences(content []byte) []byte {
	// Only complete Go syntax can distinguish a runtime reference from a
	// literal configuration value. Invalid/non-Go input keeps fail-closed text
	// screening. Known token formats are never removed, including comments.
	if !bytes.Contains(content, []byte("package ")) || credentialFormats.Match(content) {
		return content
	}
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, "", content, parser.SkipObjectResolution)
	if err != nil {
		return maskEmbeddedGoBlocks(content)
	}
	return maskParsedGoFile(content, positions, file)
}

func maskEmbeddedGoBlocks(content []byte) []byte {
	result := content
	for _, delim := range [][]byte{[]byte("'''"), []byte(`"""`)} {
		var out bytes.Buffer
		rest := result
		changed := false
		for {
			start := bytes.Index(rest, delim)
			if start < 0 {
				out.Write(rest)
				break
			}
			out.Write(rest[:start+len(delim)])
			rest = rest[start+len(delim):]
			end := bytes.Index(rest, delim)
			if end < 0 {
				out.Write(rest)
				break
			}
			block := rest[:end]
			if bytes.Contains(block, []byte("package ")) {
				positions := token.NewFileSet()
				if file, err := parser.ParseFile(positions, "", block, parser.SkipObjectResolution); err == nil {
					block = maskParsedGoFile(block, positions, file)
					changed = true
				}
			} else if bytes.Contains(block, []byte("func ")) {
				const header = "package main\n"
				synthetic := append([]byte(header), block...)
				positions := token.NewFileSet()
				if file, err := parser.ParseFile(positions, "", synthetic, parser.SkipObjectResolution); err == nil {
					masked := maskParsedGoFile(synthetic, positions, file)
					if after, ok := bytes.CutPrefix(masked, []byte(header)); ok {
						block = after
						changed = true
					}
				}
			}
			out.Write(block)
			out.Write(delim)
			rest = rest[end+len(delim):]
		}
		if changed {
			result = out.Bytes()
		}
	}
	return result
}

func maskParsedGoFile(content []byte, positions *token.FileSet, file *ast.File) []byte {
	type span struct{ start, end int }
	var references []span
	info := &types.Info{Uses: make(map[*ast.Ident]types.Object), Defs: make(map[*ast.Ident]types.Object)}
	// Imports and complete type information may be unavailable. A failed
	// type-check never authorizes an unresolved identifier as a reference.
	checker := types.Config{Error: func(error) {}}
	_, _ = checker.Check("credential-screen", positions, []*ast.File{file}, info)
	values := goInitializers(file, info)
	add := func(name string, value ast.Expr) {
		if !credentialJSONField.MatchString(name) || !goReferenceOnly(value, info, values, make(map[types.Object]bool), 0) {
			return
		}
		start, end := positions.Position(value.Pos()).Offset, positions.Position(value.End()).Offset
		if start >= 0 && end > start && end <= len(content) {
			references = append(references, span{start, end})
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.KeyValueExpr:
			name := ""
			switch key := node.Key.(type) {
			case *ast.Ident:
				name = key.Name
			case *ast.BasicLit:
				if key.Kind == token.STRING {
					name, _ = strconv.Unquote(key.Value)
				}
			}
			add(name, node.Value)
		case *ast.AssignStmt:
			if len(node.Lhs) == len(node.Rhs) {
				for i, left := range node.Lhs {
					add(goReferenceName(left), node.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			if len(node.Names) == len(node.Values) {
				for i, name := range node.Names {
					add(name.Name, node.Values[i])
				}
			}
		}
		return true
	})
	if len(references) == 0 {
		return content
	}
	slices.SortFunc(references, func(a, b span) int { return a.start - b.start })
	var masked bytes.Buffer
	last := 0
	for _, reference := range references {
		if reference.start < last {
			continue
		}
		masked.Write(content[last:reference.start])
		masked.WriteString(`""`)
		last = reference.end
	}
	masked.Write(content[last:])
	return masked.Bytes()
}

func goReferenceName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		return value.Sel.Name
	default:
		return ""
	}
}

func goReferenceOnly(expression ast.Expr, info *types.Info, values map[types.Object][]ast.Expr, seen map[types.Object]bool, depth int) bool {
	if depth > 16 {
		return false
	}
	switch value := expression.(type) {
	case *ast.SelectorExpr:
		return goReferenceOnly(value.X, info, values, seen, depth+1)
	case *ast.CallExpr:
		return slices.ContainsFunc(value.Args, func(arg ast.Expr) bool {
			return goReferenceOnly(arg, info, values, seen, depth+1)
		})
	case *ast.Ident:
		object := info.Uses[value]
		if _, variable := object.(*types.Var); !variable || seen[object] {
			return false
		}
		seen[object] = true
		defer delete(seen, object)
		for _, assigned := range values[object] {
			if !goReferenceOnly(assigned, info, values, seen, depth+1) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func goInitializers(file *ast.File, info *types.Info) map[types.Object][]ast.Expr {
	values := make(map[types.Object][]ast.Expr)
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.ValueSpec:
			if len(node.Names) == len(node.Values) {
				for i, name := range node.Names {
					if object := info.Defs[name]; object != nil {
						values[object] = append(values[object], node.Values[i])
					}
				}
			}
		case *ast.AssignStmt:
			if len(node.Lhs) == len(node.Rhs) {
				for i, left := range node.Lhs {
					if name, ok := left.(*ast.Ident); ok {
						object := info.Defs[name]
						if object == nil {
							object = info.Uses[name]
						}
						if object != nil {
							values[object] = append(values[object], node.Rhs[i])
						}
					}
				}
			}
		}
		return true
	})
	return values
}

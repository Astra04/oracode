package oracode

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"

	"github.com/dave/dst"
)

type SurgicalOps struct {
	idx *Index
}

type SurgicalEditResult struct {
	File     string    `json:"file"`
	Symbol   string    `json:"symbol,omitempty"`
	Changed  bool      `json:"changed"`
	Findings []Finding `json:"findings,omitempty"`
}

func NewSurgicalOps(idx *Index) *SurgicalOps {
	return &SurgicalOps{idx: idx}
}

func (s *SurgicalOps) ReplaceSymbol(file, name, replacement string) (SurgicalEditResult, error) {
	absPath, err := s.idx.Policy.ResolveWorkspacePath(file)
	if err != nil {
		return SurgicalEditResult{}, err
	}
	src, err := os.ReadFile(absPath)
	if err != nil {
		return SurgicalEditResult{}, fmt.Errorf("read %s: %w", file, err)
	}
	fileNode, err := SafeDstParse(src)
	if err != nil {
		return SurgicalEditResult{}, fmt.Errorf("parse %s: %w", file, err)
	}
	newDecls, err := parseDeclSnippet(replacement)
	if err != nil {
		return SurgicalEditResult{}, err
	}
	if len(newDecls) == 0 {
		return SurgicalEditResult{}, fmt.Errorf("replacement produced no declarations")
	}

	changed := false
	var outDecls []dst.Decl
	for _, decl := range fileNode.Decls {
		if matchesGoDeclName(decl, name) {
			outDecls = append(outDecls, newDecls...)
			changed = true
			continue
		}
		outDecls = append(outDecls, decl)
	}
	if !changed {
		return SurgicalEditResult{}, fmt.Errorf("symbol %q not found in %s", name, file)
	}
	fileNode.Decls = outDecls

	var buf bytes.Buffer
	if err := SafeDstFprint(&buf, fileNode); err != nil {
		return SurgicalEditResult{}, fmt.Errorf("print %s: %w", file, err)
	}
	if err := os.WriteFile(absPath, buf.Bytes(), 0o644); err != nil {
		return SurgicalEditResult{}, fmt.Errorf("write %s: %w", file, err)
	}
	return SurgicalEditResult{
		File:    file,
		Symbol:  name,
		Changed: true,
		Findings: []Finding{{
			File:    file,
			Rule:    "surgical.replace_symbol",
			Message: fmt.Sprintf("replaced %q", name),
		}},
	}, nil
}

func (s *SurgicalOps) AddStructField(file, structName, fieldSrc string) (SurgicalEditResult, error) {
	return s.mutateStructField(file, structName, fieldSrc, true)
}

func (s *SurgicalOps) RemoveStructField(file, structName, fieldName string) (SurgicalEditResult, error) {
	return s.mutateStructField(file, structName, fieldName, false)
}

func (s *SurgicalOps) mutateStructField(file, structName, payload string, add bool) (SurgicalEditResult, error) {
	absPath, err := s.idx.Policy.ResolveWorkspacePath(file)
	if err != nil {
		return SurgicalEditResult{}, err
	}
	src, err := os.ReadFile(absPath)
	if err != nil {
		return SurgicalEditResult{}, fmt.Errorf("read %s: %w", file, err)
	}
	fileNode, err := SafeDstParse(src)
	if err != nil {
		return SurgicalEditResult{}, fmt.Errorf("parse %s: %w", file, err)
	}

	var changed bool
	for _, decl := range fileNode.Decls {
		gen, ok := decl.(*dst.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*dst.TypeSpec)
			if !ok || ts.Name == nil || ts.Name.Name != structName {
				continue
			}
			st, ok := ts.Type.(*dst.StructType)
			if !ok {
				return SurgicalEditResult{}, fmt.Errorf("%s is not a struct", structName)
			}
			if add {
				fields, err := parseStructFields(payload)
				if err != nil {
					return SurgicalEditResult{}, err
				}
				st.Fields.List = append(st.Fields.List, fields...)
				changed = true
			} else {
				filtered := make([]*dst.Field, 0, len(st.Fields.List))
				for _, f := range st.Fields.List {
					if !fieldMatchesName(f, payload) {
						filtered = append(filtered, f)
						continue
					}
					changed = true
				}
				st.Fields.List = filtered
			}
		}
	}
	if !changed {
		if add {
			return SurgicalEditResult{}, fmt.Errorf("struct %q not found or field unchanged", structName)
		}
		return SurgicalEditResult{}, fmt.Errorf("field %q not found in struct %q", payload, structName)
	}

	var buf bytes.Buffer
	if err := SafeDstFprint(&buf, fileNode); err != nil {
		return SurgicalEditResult{}, fmt.Errorf("print %s: %w", file, err)
	}
	if err := os.WriteFile(absPath, buf.Bytes(), 0o644); err != nil {
		return SurgicalEditResult{}, fmt.Errorf("write %s: %w", file, err)
	}
	rule := "surgical.remove_struct_field"
	msg := fmt.Sprintf("removed field %q from struct %q", payload, structName)
	if add {
		rule = "surgical.add_struct_field"
		msg = fmt.Sprintf("added field to struct %q", structName)
	}
	return SurgicalEditResult{
		File:    file,
		Symbol:  structName,
		Changed: true,
		Findings: []Finding{{
			File:    file,
			Rule:    rule,
			Message: msg,
		}},
	}, nil
}

// SymbolSignature returns a short signature string for a named declaration in file.
// It uses go/parser (not dst) to avoid nil-pointer panics in dst@v0.27.3's
// decorator when it encounters certain Go syntax constructs (generics,
// range-over-int, etc.). go/parser is thread-safe and needs no external mutex.
func (s *SurgicalOps) SymbolSignature(file, name string) (string, error) {
	absPath, err := s.idx.Policy.ResolveWorkspacePath(file)
	if err != nil {
		return "", err
	}
	src, err := os.ReadFile(absPath)
	if err != nil {
		return "", err
	}
	// Use the standard go/parser — it handles all valid Go syntax and is
	// inherently concurrent-safe (each call gets its own FileSet).
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, absPath, src, parser.ParseComments)
	if err != nil {
		return "", err
	}

	for _, decl := range parsed.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name != nil && d.Name.Name == name {
				return formatAstFuncSignature(d), nil
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					if sp.Name != nil && sp.Name.Name == name {
						return formatAstTypeSignature(sp), nil
					}
				case *ast.ValueSpec:
					for _, ident := range sp.Names {
						if ident != nil && ident.Name == name {
							return formatAstValueSignature(d.Tok.String(), sp), nil
						}
					}
				}
			}
		}
	}
	return "", fmt.Errorf("symbol %q not found in %s", name, file)
}

// formatAstFuncSignature returns a short signature for a go/ast function declaration.
func formatAstFuncSignature(fn *ast.FuncDecl) string {
	return fmt.Sprintf("func %s(...)", fn.Name.Name)
}

// formatAstTypeSignature returns a short signature for a go/ast type declaration.
func formatAstTypeSignature(ts *ast.TypeSpec) string {
	switch ts.Type.(type) {
	case *ast.StructType:
		return fmt.Sprintf("type %s struct{...}", ts.Name.Name)
	case *ast.InterfaceType:
		return fmt.Sprintf("type %s interface{...}", ts.Name.Name)
	default:
		return fmt.Sprintf("type %s", ts.Name.Name)
	}
}

// formatAstValueSignature returns a short signature for a go/ast const or var declaration.
func formatAstValueSignature(tok string, vs *ast.ValueSpec) string {
	names := make([]string, 0, len(vs.Names))
	for _, n := range vs.Names {
		if n != nil {
			names = append(names, n.Name)
		}
	}
	return fmt.Sprintf("%s %s", tok, strings.Join(names, ", "))
}

func (s *SurgicalOps) SymbolBody(file, name string) (string, error) {
	absPath, err := s.idx.Policy.ResolveWorkspacePath(file)
	if err != nil {
		return "", err
	}
	src, err := os.ReadFile(absPath)
	if err != nil {
		return "", err
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, absPath, src, parser.ParseComments)
	if err != nil {
		return "", err
	}
	var fnDecl *ast.FuncDecl
	ast.Inspect(parsed, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != name || fn.Body == nil {
			return true
		}
		fnDecl = fn
		return false
	})
	if fnDecl != nil && fnDecl.Body != nil {
		lines := strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")
		start := fset.Position(fnDecl.Body.Lbrace).Line
		end := fset.Position(fnDecl.Body.Rbrace).Line
		if start < 1 {
			start = 1
		}
		if end > len(lines) {
			end = len(lines)
		}
		if start <= end && start <= len(lines) {
			return strings.Join(lines[start-1:end], "\n"), nil
		}
	}
	return "", fmt.Errorf("function %q not found in %s", name, file)
}

func parseDeclSnippet(snippet string) ([]dst.Decl, error) {
	wrapped := "package surgical\n\n" + strings.TrimSpace(snippet) + "\n"
	fileNode, err := SafeDstParse([]byte(wrapped))
	if err != nil {
		return nil, fmt.Errorf("parse replacement snippet: %w", err)
	}
	return fileNode.Decls, nil
}

func parseStructFields(snippet string) ([]*dst.Field, error) {
	wrapped := "package surgical\n\ntype _ struct {\n" + strings.TrimSpace(snippet) + "\n}\n"
	fileNode, err := SafeDstParse([]byte(wrapped))
	if err != nil {
		return nil, fmt.Errorf("parse struct field snippet: %w", err)
	}
	for _, decl := range fileNode.Decls {
		gen, ok := decl.(*dst.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*dst.TypeSpec)
			if !ok {
				continue
			}
			st, ok := ts.Type.(*dst.StructType)
			if !ok {
				continue
			}
			return st.Fields.List, nil
		}
	}
	return nil, fmt.Errorf("no struct fields parsed")
}

func fieldMatchesName(field *dst.Field, name string) bool {
	for _, ident := range field.Names {
		if ident != nil && ident.Name == name {
			return true
		}
	}
	return false
}

func matchesGoDeclName(decl dst.Decl, name string) bool {
	switch d := decl.(type) {
	case *dst.FuncDecl:
		return d.Name != nil && d.Name.Name == name
	case *dst.GenDecl:
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *dst.TypeSpec:
				if s.Name != nil && s.Name.Name == name {
					return true
				}
			case *dst.ValueSpec:
				for _, ident := range s.Names {
					if ident != nil && ident.Name == name {
						return true
					}
				}
			}
		}
	}
	return false
}

func formatFuncSignature(fn *dst.FuncDecl) string {
	return fmt.Sprintf("func %s(...)", fn.Name.Name)
}

func formatTypeSignature(ts *dst.TypeSpec) string {
	switch ts.Type.(type) {
	case *dst.StructType:
		return fmt.Sprintf("type %s struct{...}", ts.Name.Name)
	case *dst.InterfaceType:
		return fmt.Sprintf("type %s interface{...}", ts.Name.Name)
	default:
		return fmt.Sprintf("type %s", ts.Name.Name)
	}
}

func formatValueSignature(tok string, vs *dst.ValueSpec) string {
	var out strings.Builder
	fmt.Fprintf(&out, "%s ", tok)
	names := make([]string, 0, len(vs.Names))
	for _, n := range vs.Names {
		if n != nil {
			names = append(names, n.Name)
		}
	}
	out.WriteString(strings.Join(names, ", "))
	return out.String()
}

func exprString(expr dst.Expr) string {
	switch e := expr.(type) {
	case *dst.Ident:
		if e == nil {
			return "<ident>"
		}
		return e.Name
	case *dst.SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	case *dst.StarExpr:
		return "*" + exprString(e.X)
	case *dst.ArrayType:
		if e.Len == nil {
			return "[]" + exprString(e.Elt)
		}
		return "[" + exprString(e.Len) + "]" + exprString(e.Elt)
	case *dst.MapType:
		return "map[" + exprString(e.Key) + "]" + exprString(e.Value)
	case *dst.InterfaceType:
		return "interface{}"
	case *dst.StructType:
		return "struct{...}"
	case *dst.FuncType:
		params := fieldListBlock(e.Params)
		results := ""
		if e.Results != nil && len(e.Results.List) > 0 {
			results = " " + fieldListBlock(e.Results)
		}
		return "func" + params + results
	case *dst.Ellipsis:
		return "..." + exprString(e.Elt)
	case *dst.ParenExpr:
		return "(" + exprString(e.X) + ")"
	case *dst.BasicLit:
		if e == nil {
			return "<lit>"
		}
		return e.Value
	case *dst.IndexExpr:
		return exprString(e.X) + "[" + exprString(e.Index) + "]"
	case *dst.IndexListExpr:
		var elems []string
		for _, idx := range e.Indices {
			elems = append(elems, exprString(idx))
		}
		return exprString(e.X) + "[" + strings.Join(elems, ", ") + "]"
	default:
		return "<expr>"
	}
}

func fieldListBlock(list *dst.FieldList) string {
	if list == nil || len(list.List) == 0 {
		return "()"
	}
	parts := make([]string, 0, len(list.List))
	for _, field := range list.List {
		if field == nil {
			continue
		}
		parts = append(parts, fieldString(field))
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func fieldString(field *dst.Field) string {
	if field == nil {
		return "<field>"
	}
	var out strings.Builder
	if len(field.Names) > 0 {
		names := make([]string, 0, len(field.Names))
		for _, name := range field.Names {
			if name != nil {
				names = append(names, name.Name)
			}
		}
		out.WriteString(strings.Join(names, ", "))
		if field.Type != nil {
			out.WriteString(" ")
		}
	}
	if field.Type != nil {
		out.WriteString(exprString(field.Type))
	}
	if field.Tag != nil {
		if out.Len() > 0 {
			out.WriteString(" ")
		}
		out.WriteString(field.Tag.Value)
	}
	return out.String()
}

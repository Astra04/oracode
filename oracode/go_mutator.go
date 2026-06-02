package oracode

import (
	"bytes"
	"fmt"
	"go/token"
	"strings"

	"github.com/dave/dst"
	"github.com/dave/dst/dstutil"
)

type GoMutationResult struct {
	Source   []byte
	Changed  bool
	Findings []Finding
}

func MutateGoSource(filename string, src []byte) (GoMutationResult, error) {
	file, err := SafeDstParse(src)
	if err != nil {
		return GoMutationResult{}, fmt.Errorf("parse go dst: %w", err)
	}

	result := GoMutationResult{Source: src}

	dstutil.Apply(file, func(c *dstutil.Cursor) bool {
		node := c.Node()
		if node == nil {
			return true
		}

		switch n := node.(type) {
		case *dst.Field:
			mutateIDField(filename, n, &result)
		case *dst.BasicLit:
			if n.Kind == token.STRING && strings.Contains(strings.ToLower(n.Value), "::uuid") {
				n.Value = strings.ReplaceAll(n.Value, "::uuid", "")
				n.Value = strings.ReplaceAll(n.Value, "::UUID", "")
				result.Changed = true
				result.Findings = append(result.Findings, Finding{
					File:    filename,
					Rule:    "db.no_uuid_cast",
					Message: "removed SQL uuid cast from string literal",
				})
			}
		case *dst.CallExpr:
			flagUnwrappedErrorf(filename, n, &result)
		}

		return true
	}, nil)

	var buf bytes.Buffer
	if err := SafeDstFprint(&buf, file); err != nil {
		return GoMutationResult{}, fmt.Errorf("print go dst: %w", err)
	}
	result.Source = buf.Bytes()
	return result, nil
}

func mutateIDField(filename string, field *dst.Field, result *GoMutationResult) {
	hasID := false
	for _, name := range field.Names {
		if name.Name == "ID" {
			hasID = true
			break
		}
	}
	if !hasID {
		return
	}

	if selector, ok := field.Type.(*dst.SelectorExpr); ok {
		if pkg, ok := selector.X.(*dst.Ident); ok && pkg.Name == "uuid" && selector.Sel.Name == "UUID" {
			field.Type = dst.NewIdent("string")
			result.Changed = true
			result.Findings = append(result.Findings, Finding{
				File:    filename,
				Rule:    "db.pbuuid_id",
				Message: "changed ID field from uuid.UUID to string",
			})
		}
	}

	const expectedTag = "`db:\"id\" json:\"id\"`"
	if field.Tag == nil || field.Tag.Value != expectedTag {
		field.Tag = &dst.BasicLit{Kind: token.STRING, Value: expectedTag}
		result.Changed = true
		result.Findings = append(result.Findings, Finding{
			File:    filename,
			Rule:    "db.pbuuid_tag",
			Message: "standardized ID field tags",
		})
	}
}

func flagUnwrappedErrorf(filename string, call *dst.CallExpr, result *GoMutationResult) {
	selector, ok := call.Fun.(*dst.SelectorExpr)
	if !ok || selector.Sel.Name != "Errorf" {
		return
	}

	pkg, ok := selector.X.(*dst.Ident)
	if !ok || pkg.Name != "fmt" {
		return
	}

	for _, arg := range call.Args {
		lit, ok := arg.(*dst.BasicLit)
		if ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "%w") {
			return
		}
	}

	call.Decs.Start.Append("// ORACODE_WARN: fmt.Errorf should wrap underlying errors with %w when an error is available.")
	result.Changed = true
	result.Findings = append(result.Findings, Finding{
		File:    filename,
		Rule:    "errors.wrap",
		Message: "annotated fmt.Errorf without a %w wrapping verb",
	})
}

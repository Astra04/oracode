package oracode

import (
	"go/ast"
	"go/parser"
	"go/token"
)

// EffectModality captures what a symbol does, can do, might do, must do, or won't do.
type EffectModality struct {
	Error      string `json:"error"`      // "will", "can", "might", "wont"
	IO         string `json:"io"`         // "will", "can", "wont"
	Panic      string `json:"panic"`      // "will", "might", "wont"
	Async      string `json:"async"`      // "might", "wont"
	Security   string `json:"security"`   // "must", "can", "wont"
	Resource   string `json:"resource"`   // "must", "can", "wont"
	Confidence int    `json:"confidence"` // 0-100
}

// extractModalitiesFromCST performs heuristic analysis on a function's AST.
// Returns nil if the function cannot be found or if CST is not Go.
func extractModalitiesFromCST(cst *CST, funcName string) *EffectModality {
	if cst == nil || cst.Language != LanguageGo {
		return nil
	}

	// Use standard go/parser directly on the source bytes — no DST needed here.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, cst.Path, cst.Source, 0)
	if err != nil {
		return nil
	}

	var foundFunc *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == funcName {
			foundFunc = fn
			break
		}
	}
	if foundFunc == nil {
		return nil
	}

	effects := &EffectModality{
		Error:      "wont",
		IO:         "wont",
		Panic:      "wont",
		Async:      "wont",
		Security:   "wont",
		Resource:   "wont",
		Confidence: 70,
	}

	// 1. Return signature — check if the function returns an error
	if foundFunc.Type.Results != nil {
		for _, field := range foundFunc.Type.Results.List {
			if ident, ok := field.Type.(*ast.Ident); ok && ident.Name == "error" {
				effects.Error = "can"
				break
			}
		}
	}

	// 2. Inspect body for behavioural signals
	if foundFunc.Body != nil {
		ast.Inspect(foundFunc.Body, func(n ast.Node) bool {
			switch stmt := n.(type) {
			case *ast.GoStmt:
				effects.Async = "might"
			case *ast.DeferStmt:
				if call, ok := stmt.Call.Fun.(*ast.SelectorExpr); ok && call.Sel.Name == "Close" {
					effects.Resource = "must"
				}
				if ident, ok := stmt.Call.Fun.(*ast.Ident); ok && ident.Name == "recover" {
					effects.Panic = "might"
				}
			case *ast.CallExpr:
				if sel, ok := stmt.Fun.(*ast.SelectorExpr); ok {
					if ident, ok := sel.X.(*ast.Ident); ok {
						if ident.Name == "os" || ident.Name == "ioutil" || ident.Name == "net" {
							effects.IO = "will"
						}
						if sel.Sel.Name == "ResolveWorkspacePath" || sel.Sel.Name == "Clean" {
							effects.Security = "must"
						}
					}
				}
				if ident, ok := stmt.Fun.(*ast.Ident); ok && ident.Name == "panic" {
					effects.Panic = "will"
				}
			}
			return true
		})
	}

	return effects
}

package oracode

import (
	"go/ast"
	"go/parser"
	"go/token"
)

// checkGoTransactionRule ensures every function with *sql.Tx parameter contains defer tx.Rollback()
func checkGoTransactionRule(src string) (bool, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, 0)
	if err != nil {
		return false, err
	}
	hasTxParam := false
	hasDeferRollback := false
	ast.Inspect(file, func(n ast.Node) bool {
		switch fn := n.(type) {
		case *ast.FuncDecl:
			if fn.Type.Params != nil {
				for _, field := range fn.Type.Params.List {
					if isTxType(field.Type) {
						hasTxParam = true
						// Check body for defer tx.Rollback()
						if fn.Body != nil {
							ast.Inspect(fn.Body, func(n2 ast.Node) bool {
								if deferStmt, ok := n2.(*ast.DeferStmt); ok {
									if call, ok := deferStmt.Call.Fun.(*ast.SelectorExpr); ok {
										if ident, ok := call.X.(*ast.Ident); ok && ident.Name == "tx" && call.Sel.Name == "Rollback" {
											hasDeferRollback = true
											return false
										}
									}
								}
								return true
							})
						}
						return false
					}
				}
			}
		}
		return true
	})
	if hasTxParam && !hasDeferRollback {
		return false, nil
	}
	return true, nil
}

func isTxType(expr ast.Expr) bool {
	if star, ok := expr.(*ast.StarExpr); ok {
		if sel, ok := star.X.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "sql" && sel.Sel.Name == "Tx" {
				return true
			}
		}
	}
	return false
}

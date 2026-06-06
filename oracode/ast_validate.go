package oracode

import (
	"go/parser"
	"go/token"
)

// validateGoSyntax parses a Go file using the standard library parser to check for syntax errors.
// It returns an error if the file does not compile.
func validateGoSyntax(absPath string) error {
	fset := token.NewFileSet()
	_, err := parser.ParseFile(fset, absPath, nil, parser.AllErrors)
	return err
}

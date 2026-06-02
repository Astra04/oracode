package oracode

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"

	"golang.org/x/tools/imports"
)

// atomicWriteFile writes content to a temporary file and renames it atomically.
// If the file already exists, it is overwritten atomically.
func atomicWriteFile(path string, content []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path))
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(content); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// removeUnusedImports scans a Go file, removes unused imports using goimports,
// and rewrites the file. Returns the list of removed import paths.
func removeUnusedImports(filePath string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filePath, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse file: %w", err)
	}

	// Read original source
	src, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	// Process with goimports to remove unused imports
	processed, err := imports.Process(filePath, src, &imports.Options{
		Comments:   true,
		TabIndent:  true,
		TabWidth:   8,
		FormatOnly: false,
	})
	if err != nil {
		return nil, fmt.Errorf("goimports processing failed: %w", err)
	}

	// If no change, return early
	if bytes.Equal(src, processed) {
		return nil, nil
	}

	// Write back the cleaned file
	if err := os.WriteFile(filePath, processed, 0644); err != nil {
		return nil, fmt.Errorf("write file: %w", err)
	}

	// Detect which imports were removed by comparing ASTs
	f2, err := parser.ParseFile(fset, filePath, processed, parser.ParseComments)
	if err != nil {
		return nil, nil // can't determine, but file is cleaned
	}

	var removed []string
	for _, imp1 := range f.Imports {
		found := false
		for _, imp2 := range f2.Imports {
			samePath := imp1.Path.Value == imp2.Path.Value
			sameName := (imp1.Name == nil && imp2.Name == nil) || (imp1.Name != nil && imp2.Name != nil && imp1.Name.Name == imp2.Name.Name)
			if samePath && sameName {
				found = true
				break
			}
		}
		if !found {
			removed = append(removed, imp1.Path.Value)
		}
	}
	return removed, nil
}

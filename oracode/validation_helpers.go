package oracode

import (
	"github.com/odvcencio/gotreesitter"
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// runValidateOnFiles loads constraints and checks each changed file.
func (s *MCPServer) runValidateOnFiles(files map[string]bool) []string {
	constraints, err := LoadConstraints(s.idx.Policy.WorkspaceRoot)
	if err != nil {
		return []string{fmt.Sprintf("failed to load constraints: %v", err)}
	}
	var violations []string
	for file := range files {
		abs, err := s.idx.Policy.ResolveWorkspacePath(file)
		if err != nil {
			continue
		}
		src, err := os.ReadFile(abs)
		if err != nil {
			continue
		}

		// 1. Language-based constraints
		lang, ok := DetectOraLanguage(file)
		if ok && len(constraints) > 0 {
			res := ValidateCode(string(src), string(lang), constraints)
			if len(res) > 0 {
				violations = append(violations, fmt.Sprintf("%s: %s", file, strings.Join(res, "; ")))
			}
		}

		// 2. CSS validation (for .css and .vue files)
		if strings.HasSuffix(file, ".css") {
			cssViolations := ValidateCSS(string(src), file)
			for _, v := range cssViolations {
				violations = append(violations, fmt.Sprintf("%s:%d %s", file, v.Line, v.Message))
			}
		} else if strings.HasSuffix(file, ".vue") {
			blocks, err := ParseVueSFC(bytes.NewReader(src))
			if err == nil {
				for _, b := range blocks {
					if b.Tag == "style" {
						cssViolations := ValidateCSS(b.Content, file)
						for _, v := range cssViolations {
							// line numbers in ValidateCSS will be relative to block content,
							// we could offset them, but for now just report them.
							violations = append(violations, fmt.Sprintf("%s (style block):%d %s", file, v.Line, v.Message))
						}
					}
				}
			}
		}

		// 3. Tree-sitter syntax check for JS/TS/Vue
		if ok && (lang == LanguageTypeScript || lang == LanguageJavaScript || lang == LanguageTSX || lang == LanguageVue) {
			cst, err := s.idx.Pool.ParseFile(abs, lang, s.idx.Policy)
			if err == nil {
				if cst.HasError() {
					// Walk the tree to find the first ERROR node to report line numbers
					root := cst.Tree.RootNode()
					if root != nil {
						var walk func(n *gotreesitter.Node)
						walk = func(n *gotreesitter.Node) {
							if n.Type(cst.Tree.Language()) == "ERROR" {
								startByte := n.StartByte()
								if startByte > uint32(len(src)) {
									startByte = uint32(len(src))
								}
								line := strings.Count(string(src[:startByte]), "\n") + 1
								violations = append(violations, fmt.Sprintf("%s:%d syntax error detected by parser", file, line))
							}
							for i := 0; i < n.ChildCount(); i++ {
								walk(n.Child(i))
							}
						}
						walk(root)
					} else {
						violations = append(violations, fmt.Sprintf("%s: syntax error detected by parser", file))
					}
				}
				cst.Release()
			}
		}
	}
	return violations
}

// validateWiringConstraints checks changed files against wiring rules defined in constraints.yaml.
func (s *MCPServer) validateWiringConstraints(files map[string]bool) ([]string, error) {
	constraints, err := LoadConstraints(s.idx.Policy.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	if len(constraints) == 0 {
		return nil, nil
	}
	var violations []string
	for file := range files {
		abs, err := s.idx.Policy.ResolveWorkspacePath(file)
		if err != nil {
			continue
		}
		src, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		for _, c := range constraints {
			if strings.HasPrefix(c.ID, "wiring-") {
				matched, err := matchConstraint(c, string(src))
				if err != nil {
					continue
				}
				if c.Required && !matched {
					violations = append(violations, fmt.Sprintf("%s: %s", file, c.Message))
				} else if !c.Required && matched {
					violations = append(violations, fmt.Sprintf("%s: %s (should not match)", file, c.Message))
				}
			}
		}
	}
	return violations, nil
}

// crossDomainValidate performs three consistency checks:
// A: Go struct -> SQL table
// B: Handler -> Vue component (using module_contracts)
// C: SQL table -> Go struct
func (s *MCPServer) crossDomainValidate(changedFiles map[string]bool) []string {
	var violations []string

	// Load SQL graph
	sqlGraph, err := LoadSQLGraph(workspaceStatePath(s.idx.Policy.WorkspaceRoot, "sql_graph.json"))
	if err != nil || sqlGraph == nil {
		return violations // skip if no SQL graph
	}

	// Helper to get module name for a file
	getModule := func(file string) string {
		if s.store == nil {
			return ""
		}
		info, ok, _ := s.store.ModuleOwnerForPath(file)
		if ok {
			return info.Name
		}
		return ""
	}

	// Collect all struct names from changed Go files
	type structInfo struct {
		file   string
		name   string
		module string
	}
	var structs []structInfo
	for file := range changedFiles {
		if !strings.HasSuffix(file, ".go") {
			continue
		}
		abs, err := s.idx.Policy.ResolveWorkspacePath(file)
		if err != nil {
			continue
		}
		src, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, "", src, 0)
		if err != nil {
			continue
		}
		module := getModule(file)
		for _, decl := range parsed.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if _, ok := ts.Type.(*ast.StructType); ok {
					structs = append(structs, structInfo{file: file, name: ts.Name.Name, module: module})
				}
			}
		}
	}

	// Check A: Go struct -> SQL table
	for _, st := range structs {
		found := false
		for tableName := range sqlGraph.Tables {
			if strings.EqualFold(tableName, st.name) {
				found = true
				break
			}
		}
		if !found && st.module != "" {
			violations = append(violations, fmt.Sprintf("%s: struct %q has no matching SQL table (module %s)", st.file, st.name, st.module))
		}
	}

	// Check C: SQL table -> Go struct (reverse)
	for tableName, tableDef := range sqlGraph.Tables {
		if tableDef.Module == "" {
			continue
		}
		found := false
		for _, st := range structs {
			if strings.EqualFold(st.name, tableName) && st.module == tableDef.Module {
				found = true
				break
			}
		}
		if !found {
			// Only flag if the module has changed files
			moduleChanged := false
			for file := range changedFiles {
				if getModule(file) == tableDef.Module {
					moduleChanged = true
					break
				}
			}
			if moduleChanged {
				violations = append(violations, fmt.Sprintf("SQL table %q (module %s) has no matching Go struct", tableName, tableDef.Module))
			}
		}
	}

	// Check B: Handler -> Vue component (load module contracts)
	contractsDir := workspaceSourcePath(s.idx.Policy.WorkspaceRoot, "module_contracts")
	entries, err := os.ReadDir(contractsDir)
	if err != nil {
		return violations
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(contractsDir, entry.Name()))
		if err != nil {
			continue
		}
		var contract struct {
			Name          string `json:"name"`
			VueComponents []struct {
				Handler   string `json:"handler"`
				Component string `json:"component"`
			} `json:"vue_components"`
		}
		if err := json.Unmarshal(data, &contract); err != nil {
			continue
		}
		for _, pair := range contract.VueComponents {
			handlerFile := s.findHandlerFile(pair.Handler)
			if handlerFile == "" {
				continue
			}
			if !changedFiles[handlerFile] {
				continue
			}
			compAbs, err := s.idx.Policy.ResolveWorkspacePath(pair.Component)
			if err != nil || !fileExists(compAbs) {
				violations = append(violations, fmt.Sprintf("%s: Vue component %q not found for handler %s", handlerFile, pair.Component, pair.Handler))
			}
		}
	}
	return violations
}

// fileExists checks if a file exists and is not a directory.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// findHandlerFile returns the file path where the given handler symbol is defined.
func (s *MCPServer) findHandlerFile(handlerName string) string {
	defs := s.idx.FindDefinitions(handlerName)
	if len(defs) > 0 {
		return defs[0].File
	}
	return ""
}

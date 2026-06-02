package oracode

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/odvcencio/gotreesitter"
)

type FrontendOps struct {
	idx *Index
}

func NewFrontendOps(idx *Index) *FrontendOps {
	return &FrontendOps{idx: idx}
}

// ReplaceSymbolInVue replaces a function, composable, or ref in a Vue component's script block.
func (f *FrontendOps) ReplaceSymbolInVue(file, symbol, newSource string) error {
	return f.modifyVueScript(file, func(content string) (string, error) {
		// Use tree-sitter via the parser pool for accurate symbol boundaries
		cst, err := f.idx.Pool.ParseSource(file, LanguageTypeScript, []byte(content))
		if err == nil && cst != nil && cst.Tree != nil {
			defer cst.Release()
			// Try to find the symbol using tree-sitter walk
			if start, end, ok := findNamedRange(cst, symbol, content); ok {
				return content[:start] + newSource + content[end:], nil
			}
		}
		// Fallback: regex-based replacement
		// Match function declarations: function symbol(...) or const symbol = ...
		patterns := []string{
			`^(export\s+)?(async\s+)?function\s+` + regexp.QuoteMeta(symbol) + `\s*\(`,
			`^(export\s+)?(const|let|var)\s+` + regexp.QuoteMeta(symbol) + `\s*[=:]`,
		}
		for _, p := range patterns {
			re := regexp.MustCompile(`(?m)` + p)
			loc := re.FindStringIndex(content)
			if loc == nil {
				continue
			}
			// Find the end of this declaration (next top-level declaration or EOF)
			rest := content[loc[0]:]
			endIdx := findDeclarationEnd(rest)
			if endIdx < 0 {
				endIdx = len(content)
			} else {
				endIdx = loc[0] + endIdx
			}
			return content[:loc[0]] + newSource + content[endIdx:], nil
		}
		return "", fmt.Errorf("symbol %q not found in %s", symbol, file)
	})
}

// AddImportToVue adds an ES import statement to the script block.
func (f *FrontendOps) AddImportToVue(file, importPath string) error {
	return f.modifyVueScript(file, func(content string) (string, error) {
		importLine := fmt.Sprintf("import %s;", importPath)
		// If no import block, add at top
		if !strings.Contains(content, "import") {
			return importLine + "\n" + content, nil
		}
		// Insert after last import
		lastImport := regexp.MustCompile(`(?m)^import .*$`)
		idx := lastImport.FindAllStringIndex(content, -1)
		if len(idx) == 0 {
			return importLine + "\n" + content, nil
		}
		pos := idx[len(idx)-1][1]
		return content[:pos] + "\n" + importLine + content[pos:], nil
	})
}

// AddComposableToSetup injects a composable call into the setup() function or <script setup> block.
func (f *FrontendOps) AddComposableToSetup(file, composableCall string) error {
	absPath, err := f.idx.Policy.ResolveWorkspacePath(file)
	if err != nil {
		return err
	}
	src, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}

	isScriptSetup := strings.Contains(string(src), "<script setup")

	return f.modifyVueScript(file, func(content string) (string, error) {
		if isScriptSetup {
			// For script setup, inject the composable after imports, or at the top if no imports
			lastImport := regexp.MustCompile("(?m)^import .*$")
			idx := lastImport.FindAllStringIndex(content, -1)
			if len(idx) == 0 {
				return composableCall + "\n" + content, nil
			}
			pos := idx[len(idx)-1][1]
			return content[:pos] + "\n\n" + composableCall + content[pos:], nil
		}

		// Look for export default { setup() { ... } }
		setupRe := regexp.MustCompile("setup\\s*\\(\\s*\\)\\s*\\{([^}]*(?:\\{[^}]*\\}[^}]*)*)\\}")
		if setupRe.MatchString(content) {
			replaced := setupRe.ReplaceAllStringFunc(content, func(match string) string {
				body := setupRe.FindStringSubmatch(match)[1]
				newBody := composableCall + ";\n" + body
				return strings.Replace(match, body, newBody, 1)
			})
			return replaced, nil
		}
		return content, fmt.Errorf("no setup function found")
	})
}

// modifyVueScript is a helper that extracts the script block, applies a transformation, and writes back.
func (f *FrontendOps) modifyVueScript(file string, transform func(content string) (string, error)) error {
	absPath, err := f.idx.Policy.ResolveWorkspacePath(file)
	if err != nil {
		return err
	}
	src, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}
	blocks, err := ParseVueSFC(bytes.NewReader(src))
	if err != nil {
		return err
	}
	var scriptIdx int
	var scriptBlock *SFCBlock
	for i, b := range blocks {
		if b.Tag == "script" {
			scriptIdx = i
			scriptBlock = &b
			break
		}
	}
	if scriptBlock == nil {
		return fmt.Errorf("no script block found in %s", file)
	}
	newContent, err := transform(scriptBlock.Content)
	if err != nil {
		return err
	}
	blocks[scriptIdx].Content = newContent
	newSource := ReplaceSFCBlocks(src, blocks)
	if err := os.WriteFile(absPath, newSource, 0644); err != nil {
		return err
	}
	// Reindex
	lang, _ := DetectOraLanguage(file)
	return f.idx.IndexFile(file, lang)
}

// findNamedRange walks the tree-sitter tree using the CST to find a declaration with a given name.
// Returns (startByte, endByte, found).
func findNamedRange(cst *CST, symbol string, source string) (int, int, bool) {
	if cst == nil || cst.Tree == nil || cst.Tree.RootNode() == nil {
		return 0, 0, false
	}
	return walkForSymRange(cst.Tree.RootNode(), cst.Tree.Language(), symbol, source)
}

// walkForSymRange recursively walks tree-sitter children searching for a named declaration.
func walkForSymRange(node *gotreesitter.Node, lang *gotreesitter.Language, symbol string, source string) (int, int, bool) {
	typeName := node.Type(lang)
	isDecl := typeName == "function_declaration" || typeName == "lexical_declaration" || typeName == "variable_declaration"

	if isDecl {
		// Check children for matching identifier
		for i := 0; i < node.ChildCount(); i++ {
			child := node.Child(i)
			if child == nil {
				continue
			}
			childType := child.Type(lang)
			if (childType == "identifier" || childType == "property_identifier") && child.ChildCount() == 0 {
				name := source[child.StartByte():child.EndByte()]
				if name == symbol {
					return int(node.StartByte()), int(node.EndByte()), true
				}
			}
		}
		// Also check deeper for variable_declarator name
		if typeName == "lexical_declaration" || typeName == "variable_declaration" {
			for i := 0; i < node.ChildCount(); i++ {
				child := node.Child(i)
				if child == nil {
					continue
				}
				if child.Type(lang) == "variable_declarator" {
					for j := 0; j < child.ChildCount(); j++ {
						grandchild := child.Child(j)
						if grandchild == nil {
							continue
						}
						if grandchild.Type(lang) == "identifier" && grandchild.ChildCount() == 0 {
							name := source[grandchild.StartByte():grandchild.EndByte()]
							if name == symbol {
								return int(node.StartByte()), int(node.EndByte()), true
							}
						}
					}
				}
			}
		}
	}
	// Recurse into children
	for i := 0; i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		if start, end, ok := walkForSymRange(child, lang, symbol, source); ok {
			return start, end, ok
		}
	}
	return 0, 0, false
}

// findDeclarationEnd finds the end of a declaration in source code.
// Returns the index relative to the start of the input, or -1 if no clear boundary.
func findDeclarationEnd(rest string) int {
	depth := 0
	inString := false
	stringChar := byte(0)
	for i := 0; i < len(rest); i++ {
		ch := rest[i]
		if inString {
			if ch == '\\' {
				i++ // skip escaped char
				continue
			}
			if ch == stringChar {
				inString = false
			}
			continue
		}
		switch ch {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 && i > 0 {
				// End of function body; skip any trailing whitespace/punctuation
				j := i + 1
				for j < len(rest) && (rest[j] == ' ' || rest[j] == '\t' || rest[j] == '\n' || rest[j] == '\r' || rest[j] == ',' || rest[j] == ';') {
					j++
				}
				return j
			}
		case '\'', '"', '`':
			inString = true
			stringChar = ch
		}
	}
	return -1
}

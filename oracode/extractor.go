package oracode

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/odvcencio/gotreesitter"
)

var queries = map[Language]string{
	LanguageGo: `
(function_declaration name: (identifier) @name) @func
(method_declaration name: (field_identifier) @name) @method
(type_spec name: (type_identifier) @name) @type
(const_spec name: (identifier) @name) @const
(var_spec name: (identifier) @name) @var
(import_spec path: (_) @path) @import
(call_expression function: (identifier) @call) @ref
(call_expression function: (selector_expression field: (field_identifier) @call)) @ref
	`,
	LanguageJavaScript: `
(function_declaration name: (identifier) @name) @func
(class_declaration name: (identifier) @name) @class
(variable_declarator name: (identifier) @name) @var
(import_statement source: (string) @path) @import
(call_expression function: (identifier) @call) @ref
(call_expression function: (member_expression property: (property_identifier) @call)) @ref
	`,
	LanguageTypeScript: `
(function_declaration name: (identifier) @name) @func
(class_declaration name: (identifier) @name) @class
(interface_declaration name: (identifier) @name) @interface
(type_alias_declaration name: (identifier) @name) @type
(variable_declarator name: (identifier) @name) @var
(import_statement source: (string) @path) @import
(call_expression function: (identifier) @call) @ref
(call_expression function: (member_expression property: (property_identifier) @call)) @ref
	`,
	LanguageVue: `
(element (start_tag (tag_name) @name)) @vue_tag
	`,
	LanguageSQL: `
(create_table_statement name: (identifier) @name) @table
	`,
}

// ExtractSymbols runs language-specific Tree-sitter queries.
func ExtractSymbols(cst *CST) ([]*Symbol, error) {
	if cst == nil || cst.Tree == nil || cst.Tree.RootNode() == nil {
		return nil, fmt.Errorf("invalid CST")
	}

	qStr, ok := queries[cst.Language]
	if !ok {
		return nil, nil // No queries defined for this language
	}

	q, err := gotreesitter.NewQuery(qStr, cst.Tree.Language())
	if err != nil {
		return nil, fmt.Errorf("failed to create query: %w", err)
	}

	cursor := q.Exec(cst.Tree.RootNode(), cst.Tree.Language(), cst.Source)
	var symbols []*Symbol
	var imports []string

	for {
		match, ok := cursor.NextMatch()
		if !ok {
			break
		}

		for _, capture := range match.Captures {
			node := capture.Node
			name := capture.Name

			if name == "import" || name == "path" {
				text := string(cst.Source[node.StartByte():node.EndByte()])
				text = strings.Trim(text, "\"'\x60")
				imports = append(imports, text)
				continue
			}

			if name == "name" || name == "call" {
				text := string(cst.Source[node.StartByte():node.EndByte()])
				kind := "definition"
				if name == "call" {
					kind = "reference"
				}

				point := node.StartPoint()
				row, col := point.Row, point.Column

				sym := &Symbol{
					Name:     text,
					Kind:     kind,
					File:     cst.Path,
					Line:     int(row) + 1,
					Column:   int(col) + 1,
					Language: cst.Language,
				}
				symbols = append(symbols, sym)
			}
		}
	}

	if len(imports) > 0 {
		// Attach imports to a special module symbol
		modSym := &Symbol{
			Name:     "_module_",
			Kind:     "module",
			File:     cst.Path,
			Language: cst.Language,
			Imports:  imports,
		}
		symbols = append(symbols, modSym)
	}

	// Vue <script setup> support: extract script-level definitions so component symbols are discoverable.
	if cst.Language == LanguageVue {
		symbols = append(symbols, extractVueScriptSymbols(cst)...)
	}

	return symbols, nil
}

func extractVueScriptSymbols(cst *CST) []*Symbol {
	blocks, err := ParseVueSFC(bytes.NewReader(cst.Source))
	if err != nil {
		return nil
	}

	constRe := regexp.MustCompile(`\bconst\s+([A-Za-z_][A-Za-z0-9_]*)\b`)
	fnRe := regexp.MustCompile(`\bfunction\s+([A-Za-z_][A-Za-z0-9_]*)\b`)
	exportRe := regexp.MustCompile(`\bexport\s+(?:const|function|class)\s+([A-Za-z_][A-Za-z0-9_]*)\b`)

	var out []*Symbol
	for _, block := range blocks {
		if block.Tag != "script" {
			continue
		}
		scriptSource := strings.ReplaceAll(block.Content, "\r\n", "\n")
		contentStart := block.StartOffset + len(block.OpenTagRaw)
		startLine := strings.Count(strings.ReplaceAll(string(cst.Source[:contentStart]), "\r\n", "\n"), "\n") + 1
		lines := strings.Split(scriptSource, "\n")
		for i, line := range lines {
			for _, re := range []*regexp.Regexp{exportRe, fnRe, constRe} {
				matches := re.FindAllStringSubmatch(line, -1)
				for _, m := range matches {
					if len(m) < 2 {
						continue
					}
					name := strings.TrimSpace(m[1])
					if name == "" || name == "props" || name == "emit" {
						continue
					}
					out = append(out, &Symbol{
						Name:     name,
						Kind:     "definition",
						File:     cst.Path,
						Line:     startLine + i,
						Column:   strings.Index(line, name) + 1,
						Language: LanguageVue,
					})
				}
			}
		}
	}
	return out
}

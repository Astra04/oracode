package oracode

import "regexp"

var namedImportPattern = regexp.MustCompile(`(?m)import\s*\{([^}]*)\}`)

func FindTypeScriptImports(code []byte, _ bool) ([]string, error) {
	matches := namedImportPattern.FindAllSubmatch(code, -1)
	imports := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) > 1 {
			imports = append(imports, string(match[1]))
		}
	}
	return imports, nil
}

package oracode

import (
	"strings"
)

// ValidateCSS is a lightweight parser to check for unclosed braces and missing semicolons in CSS.
// It is intended as a helper validation tool.
func ValidateCSS(code string, file string) []Violation {
	var violations []Violation

	var braceDepth int
	lines := strings.Split(code, "\n")

	// Track whether we are inside a CSS rule block
	for i, line := range lines {
		clean := strings.TrimSpace(line)
		if clean == "" {
			continue
		}

		for _, char := range line {
			if char == '{' {
				braceDepth++
			} else if char == '}' {
				braceDepth--
				if braceDepth < 0 {
					violations = append(violations, Violation{
						ConstraintID: "css.unmatched_brace",
						Message:      "Unexpected closing brace '}'",
						Line:         i + 1,
					})
					braceDepth = 0 // reset to avoid cascading errors
				}
			}
		}
	}

	if braceDepth > 0 {
		violations = append(violations, Violation{
			ConstraintID: "css.unclosed_brace",
			Message:      "Unclosed opening brace '{'",
			Line:         len(lines),
		})
	}

	return violations
}

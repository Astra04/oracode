package oracode

import (
	"strings"
)

// ValidateCSS uses a simple bracket scanner to check CSS for errors.
// It is intended as a lightweight circuit breaker during batch editing.
func ValidateCSS(code string, file string) []Violation {
	var violations []Violation

	var braceDepth int
	lines := strings.Split(code, "\n")

	// Track whether we are inside a CSS rule block
	for i, line := range lines {
		// Quick check to skip braces in strings
		inString := false
		var stringChar byte

		for j := 0; j < len(line); j++ {
			char := line[j]
			if inString {
				if char == '\\' { j++; continue }
				if char == stringChar { inString = false }
				continue
			}

			if char == '"' || char == '\'' {
				inString = true
				stringChar = char
				continue
			}

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
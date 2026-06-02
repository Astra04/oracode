package oracode

// ValidateCSS validates CSS code using a single-pass character state machine.
// It checks for brace balancing, unclosed strings, and unclosed comments.
func ValidateCSS(code string, file string) []Violation {
	var violations []Violation

	const (
		TopLevel = iota
		InBlock
		InComment
		InString
	)

	state := TopLevel
	prevState := TopLevel

	braceDepth := 0
	var stringChar byte
	lineNum := 1

	for i := 0; i < len(code); i++ {
		char := code[i]

		if char == '\n' {
			lineNum++
		}

		if state == InComment {
			if char == '*' && i+1 < len(code) && code[i+1] == '/' {
				state = prevState
				i++ // skip '/'
			}
			continue
		}

		if state == InString {
			if char == '\\' {
				i++ // skip escaped char
				if i < len(code) && code[i] == '\n' {
					lineNum++
				}
				continue
			}
			// In CSS, strings cannot contain unescaped newlines.
			if char == '\n' {
				violations = append(violations, Violation{
					ConstraintID: "css.unclosed_string_newline",
					Message:      "String literal spans multiple lines without escape",
					Line:         lineNum - 1, // error happened on the previous line
				})
				state = prevState
				continue
			}
			if char == stringChar {
				state = prevState
			}
			continue
		}

		// Handle entering strings or comments
		if char == '/' && i+1 < len(code) && code[i+1] == '*' {
			prevState = state
			state = InComment
			i++ // skip '*'
			continue
		}

		if char == '"' || char == '\'' {
			prevState = state
			state = InString
			stringChar = char
			continue
		}

		// Block transitions
		if char == '{' {
			braceDepth++
			if state == TopLevel {
				state = InBlock
			}
		} else if char == '}' {
			braceDepth--
			if braceDepth < 0 {
				violations = append(violations, Violation{
					ConstraintID: "css.unmatched_brace",
					Message:      "Unexpected closing brace '}'",
					Line:         lineNum,
				})
				braceDepth = 0 // reset to avoid cascades
			} else if braceDepth == 0 {
				state = TopLevel
			}
		}
	}

	// EOF checks
	if state == InComment {
		violations = append(violations, Violation{
			ConstraintID: "css.unclosed_comment",
			Message:      "Unclosed comment '/*'",
			Line:         lineNum,
		})
	} else if state == InString {
		violations = append(violations, Violation{
			ConstraintID: "css.unclosed_string",
			Message:      "Unclosed string literal",
			Line:         lineNum,
		})
	} else if braceDepth > 0 {
		violations = append(violations, Violation{
			ConstraintID: "css.unclosed_brace",
			Message:      "Unclosed opening brace '{'",
			Line:         lineNum,
		})
	}

	return violations
}
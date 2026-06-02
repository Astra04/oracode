package oracode

// ValidateCSS validates CSS code using a single-pass character state machine.
// It checks for brace balancing, unclosed strings, and unclosed comments.
// Returns violations with accurate line numbers pointing to the start of the malformed block.
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

	var blockOpenLines []int
	var commentOpenLine int
	var stringOpenLine int

	var stringChar byte
	lineNum := 1

	for i := 0; i < len(code); i++ {
		char := code[i]

		if char == '\n' {
			lineNum++
			// Continue processing newlines so strings can catch unescaped newlines.
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
					Line:         stringOpenLine,
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
			commentOpenLine = lineNum
			i++ // skip '*'
			continue
		}

		if char == '"' || char == '\'' {
			prevState = state
			state = InString
			stringOpenLine = lineNum
			stringChar = char
			continue
		}

		// Block transitions
		if char == '{' {
			blockOpenLines = append(blockOpenLines, lineNum)
			if state == TopLevel {
				state = InBlock
			}
		} else if char == '}' {
			if len(blockOpenLines) == 0 {
				violations = append(violations, Violation{
					ConstraintID: "css.unmatched_brace",
					Message:      "Unexpected closing brace '}'",
					Line:         lineNum,
				})
			} else {
				blockOpenLines = blockOpenLines[:len(blockOpenLines)-1]
				if len(blockOpenLines) == 0 {
					state = TopLevel
				}
			}
		}
	}

	// EOF checks
	if state == InComment {
		violations = append(violations, Violation{
			ConstraintID: "css.unclosed_comment",
			Message:      "Unclosed comment '/*'",
			Line:         commentOpenLine,
		})
	} else if state == InString {
		violations = append(violations, Violation{
			ConstraintID: "css.unclosed_string",
			Message:      "Unclosed string literal",
			Line:         stringOpenLine,
		})
	}

	// Report all unclosed braces
	for _, openLine := range blockOpenLines {
		violations = append(violations, Violation{
			ConstraintID: "css.unclosed_brace",
			Message:      "Unclosed opening brace '{'",
			Line:         openLine,
		})
	}

	return violations
}
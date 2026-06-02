// constraint_logging.go
// Instrumented wrapper for the validate_code tool.
// Violation and ValidationResult types live in constraint.go.

package oracode

import "context"

// ValidateCodeInstrumented is the logged version of the scalpel_validate_code handler.
// Emits pass/fail log lines per constraint, plus a ToolSpan wrapping the whole run.
func ValidateCodeInstrumented(ctx context.Context, snippet, module string, constraints []Constraint) ValidationResult {
	done := ToolSpan(ctx, "scalpel_validate_code",
		"module", module,
		"snippet_bytes", len(snippet),
		"constraints_loaded", len(constraints),
	)

	var violations []Violation

	for _, c := range constraints {
		matched, err := matchConstraint(c, snippet)
		if err != nil {
			continue
		}

		violated := (c.Required && !matched) || (!c.Required && matched)
		if violated {
			msg := c.Message
			LogConstraintFail(c.ID, module, msg)
			violations = append(violations, Violation{
				ConstraintID: c.ID,
				Message:      msg,
			})
		} else {
			LogConstraintPass(c.ID, module)
		}
	}

	result := ValidationResult{
		Valid:      len(violations) == 0,
		Violations: violations,
	}

	done(estimateResultSize(result), nil)
	return result
}

// estimateResultSize returns an approximate byte count of the validation result
// for the ToolSpan done() call — avoids a full JSON marshal just for logging.
func estimateResultSize(r ValidationResult) int {
	size := 50 // base JSON overhead
	for _, v := range r.Violations {
		size += len(v.ConstraintID) + len(v.Message) + 20
	}
	return size
}

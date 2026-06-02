package oracode

import (
	"context"
	"fmt"
	"strings"
)

// EvaluatePattern analyzes a code snippet against architectural constraints and
// searches for existing precedents in the codebase.
func (s *MCPServer) EvaluatePattern(snippet, language, scope string) (string, error) {
	if snippet == "" || language == "" {
		return "", fmt.Errorf("snippet and language are required")
	}

	var report strings.Builder
	report.WriteString("## Brainstorming & Pattern Evaluation Report\n\n")

	constraints, err := LoadConstraints(s.idx.Policy.WorkspaceRoot)
	if err == nil && len(constraints) > 0 {
		res := ValidateCodeInstrumented(context.Background(), snippet, language, constraints)
		if !res.Valid {
			report.WriteString("### ⚠️ Constraint Violations Detected\n")
			report.WriteString("This snippet violates the following team rules and should NOT be used as-is:\n")
			for _, v := range res.Violations {
				fmt.Fprintf(&report, "- **%s**: %s\n", v.ConstraintID, v.Message)
			}
		} else {
			report.WriteString("### ✅ Architectural Compliance\n")
			report.WriteString("This snippet passes all known team architectural constraints.\n")
		}
	} else {
		report.WriteString("### ℹ️ Architectural Compliance\n")
		report.WriteString("No active constraints loaded for validation.\n")
	}

	lines := strings.Split(snippet, "\n")
	var bestSearchTerm string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if language == "vue" || language == "ts" || language == "typescript" {
			if len(trimmed) > 8 && (strings.HasPrefix(trimmed, "const ") || strings.Contains(trimmed, "defineProps") || strings.Contains(trimmed, "watch(") || strings.Contains(trimmed, "computed(")) {
				bestSearchTerm = trimmed
				break
			}
		} else {
			if len(trimmed) > 8 && !strings.HasPrefix(trimmed, "func ") && !strings.Contains(trimmed, "if err != nil") && !strings.HasPrefix(trimmed, "package ") {
				bestSearchTerm = trimmed
				break
			}
		}
	}

	if bestSearchTerm != "" {
		searchResult, err := s.findString(bestSearchTerm, scope, language, false, 5)
		if err == nil && !strings.Contains(searchResult, "No matches found") {
			report.WriteString("\n### 🔍 Existing Precedents Found\n")
			report.WriteString("I found similar implementations already existing in the codebase. Consider aligning with or reusing these instead of inventing a new pattern:\n")
			report.WriteString(searchResult)
		} else {
			report.WriteString("\n### 🆕 Novel Pattern\n")
			report.WriteString("No exact matching precedents were found in the codebase. This appears to be a new implementation pattern.")
		}
	} else {
		report.WriteString("\n### 🆕 Novel Pattern\n")
		report.WriteString("Snippet was too generic to find specific precedents.")
	}

	return report.String(), nil
}

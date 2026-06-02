package oracode

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

type VerificationFailure struct {
	Passed      bool   `json:"passed"`
	RawOutput   string `json:"raw_output,omitempty"`
	FailingFile string `json:"failing_file,omitempty"`
	Line        int    `json:"line,omitempty"`
	ErrorMsg    string `json:"error_msg,omitempty"`
	CodeContext string `json:"code_context,omitempty"`
}

func (e *Engine) VerifyWithContext() VerificationFailure {
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = e.WorkspaceRoot
	outputBytes, err := cmd.CombinedOutput()
	output := string(outputBytes)

	if err == nil {
		return VerificationFailure{Passed: true, RawOutput: "Build successful."}
	}

	failure := VerificationFailure{
		Passed:    false,
		RawOutput: output,
	}

	// Handles both Windows absolute paths (C:\...\file.go) and relative paths (internal/module/file.go)
	re := regexp.MustCompile(`(?m)^([a-zA-Z]:\\[^\r\n:]+\.go|[a-zA-Z0-9_\-\.\/]+\.go):(\d+):(?:\d+:)?\s*(.*)$`)
	matches := re.FindStringSubmatch(output)

	if len(matches) == 4 {
		failure.FailingFile = matches[1]
		if line, err := strconv.Atoi(matches[2]); err == nil {
			failure.Line = line
		}
		failure.ErrorMsg = strings.TrimSpace(matches[3])

		absPath, resolveErr := e.resolve(failure.FailingFile)
		if resolveErr == nil {
			fileData, readErr := os.ReadFile(absPath)
			if readErr == nil {
				lines := strings.Split(string(fileData), "\n")
				start := failure.Line - 3
				end := failure.Line + 2
				if start < 0 {
					start = 0
				}
				if end > len(lines) {
					end = len(lines)
				}

				var ctxBuilder strings.Builder
				for i := start; i < end; i++ {
					prefix := "  "
					if i == failure.Line-1 {
						prefix = "> "
					}
					fmt.Fprintf(&ctxBuilder, "%s %d: %s\n", prefix, i+1, lines[i])
				}
				failure.CodeContext = ctxBuilder.String()
			}
		}
	}
	return failure
}

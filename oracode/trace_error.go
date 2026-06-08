package oracode

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type TraceErrorReport struct {
	Ref         string     `json:"ref"`
	Endpoint    string     `json:"endpoint"`
	Method      string     `json:"method"`
	ErrorMsg    string     `json:"error_msg"`
	Trace       string     `json:"trace,omitempty"`
	SchemaGap   *SchemaGap `json:"schema_gap,omitempty"`
	SourceLines []string   `json:"source_lines,omitempty"`
	Notes       []string   `json:"notes,omitempty"`
}

// TraceError fetches a watchdog error row and correlates it with route and schema context.
func (s *MCPServer) TraceError(ref string) (*TraceErrorReport, error) {
	dsn := resolveWorkspaceDSN(s.idx.Policy.WorkspaceRoot)
	if dsn == "" {
		return nil, fmt.Errorf("ORACODE_DB_DSN not set")
	}
	rows, err := queryPostgresRows(dsn, fmt.Sprintf(
		`SELECT endpoint, method, full_error FROM system_error_logs WHERE error_ref = %s ORDER BY created_at DESC LIMIT 1`,
		quoteSQLLiteral(ref),
	))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("error ref %q not found", ref)
	}
	parts := strings.SplitN(rows[0], "\t", 3)
	if len(parts) < 3 {
		return nil, fmt.Errorf("unexpected watchdog record format for %q", ref)
	}

	report := &TraceErrorReport{
		Ref:      ref,
		Endpoint: strings.TrimSpace(parts[0]),
		Method:   strings.TrimSpace(parts[1]),
		ErrorMsg: strings.TrimSpace(parts[2]),
	}

	if report.Endpoint != "" {
		trace, err := s.traceChain(report.Endpoint, report.Method, 4)
		if err == nil {
			report.Trace = trace
		} else {
			report.Notes = append(report.Notes, err.Error())
		}
	}

	lowerMsg := strings.ToLower(report.ErrorMsg)
	if strings.Contains(lowerMsg, "relation does not exist") || strings.Contains(lowerMsg, "column does not exist") {
		if gap, err := s.SchemaGap(); err == nil {
			report.SchemaGap = gap
		} else {
			report.Notes = append(report.Notes, err.Error())
		}
	}

	re := regexp.MustCompile(`([A-Za-z0-9_./\\-]+):(\d+)`)
	if m := re.FindStringSubmatch(report.ErrorMsg); len(m) == 3 {
		line, _ := strconv.Atoi(m[2])
		if snippet, err := s.sourceRange(m[1], line-4, line+8); err == nil {
			report.SourceLines = strings.Split(snippet, "\n")
		}
	}

	return report, nil
}

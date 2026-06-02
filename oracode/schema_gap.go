package oracode

import (
	"bytes"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

type SchemaGap struct {
	MissingTables  []string            `json:"missing_tables"`
	MissingColumns map[string][]string `json:"missing_columns"`
	ExtraTables    []string            `json:"extra_tables"`
}

// SchemaGap compares migration SQL (sql_graph.json) against a live PostgreSQL schema.
// It shells out to psql so the tool stays dependency-light inside this repo.
func (s *MCPServer) SchemaGap() (*SchemaGap, error) {
	dsn := resolveWorkspaceDSN(s.idx.Policy.WorkspaceRoot)
	if dsn == "" {
		return nil, fmt.Errorf("ORACODE_DB_DSN not set")
	}
	graph, err := LoadSQLGraph(workspaceStatePath(s.idx.Policy.WorkspaceRoot, "sql_graph.json"))
	if err != nil || graph == nil {
		return nil, fmt.Errorf("sql_graph.json missing: %w", err)
	}

	liveTables, err := queryPostgresRows(dsn, `SELECT table_schema || '.' || table_name FROM information_schema.tables WHERE table_type = 'BASE TABLE' ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	liveTableSet := make(map[string]struct{}, len(liveTables))
	for _, table := range liveTables {
		liveTableSet[normalizeSQLName(table)] = struct{}{}
	}

	gap := &SchemaGap{MissingColumns: make(map[string][]string)}
	graphTableSet := make(map[string]struct{}, len(graph.Tables))
	for name := range graph.Tables {
		graphTableSet[normalizeSQLName(name)] = struct{}{}
	}

	for name, def := range graph.Tables {
		norm := normalizeSQLName(name)
		if _, ok := liveTableSet[norm]; !ok {
			gap.MissingTables = append(gap.MissingTables, name)
			continue
		}
		query := fmt.Sprintf(
			`SELECT column_name, data_type, is_nullable FROM information_schema.columns WHERE table_schema || '.' || table_name = %s ORDER BY ordinal_position`,
			quoteSQLLiteral(name),
		)
		rows, err := queryPostgresRows(dsn, query)
		if err != nil {
			continue
		}
		liveCols := make(map[string]struct{}, len(rows))
		for _, row := range rows {
			parts := strings.Split(row, "\t")
			if len(parts) == 0 {
				continue
			}
			liveCols[strings.ToLower(strings.TrimSpace(parts[0]))] = struct{}{}
		}
		for _, col := range def.Columns {
			if _, ok := liveCols[strings.ToLower(col.Name)]; !ok {
				gap.MissingColumns[name] = append(gap.MissingColumns[name], col.Name)
			}
		}
	}

	for live := range liveTableSet {
		if _, ok := graphTableSet[live]; !ok {
			gap.ExtraTables = append(gap.ExtraTables, live)
		}
	}

	sort.Strings(gap.MissingTables)
	sort.Strings(gap.ExtraTables)
	for table := range gap.MissingColumns {
		sort.Strings(gap.MissingColumns[table])
	}
	return gap, nil
}

func queryPostgresRows(dsn, query string) ([]string, error) {
	cmd := exec.Command("psql", dsn, "-At", "-F", "\t", "-c", query)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("psql query failed: %w: %s", err, msg)
		}
		return nil, fmt.Errorf("psql query failed: %w", err)
	}
	raw := strings.Split(strings.ReplaceAll(stdout.String(), "\r\n", "\n"), "\n")
	var rows []string
	for _, line := range raw {
		if strings.TrimSpace(line) == "" {
			continue
		}
		rows = append(rows, strings.TrimSpace(line))
	}
	return rows, nil
}

func quoteSQLLiteral(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

func normalizeSQLName(name string) string {
	return strings.ToLower(strings.TrimSpace(strings.Trim(name, "`\"")))
}

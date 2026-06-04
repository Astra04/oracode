package oracode

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type TableColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Key      string `json:"key,omitempty"`
}

type ForeignKey struct {
	Column    string `json:"column"`
	RefTable  string `json:"ref_table"`
	RefColumn string `json:"ref_column"`
}

type TableDef struct {
	Name        string        `json:"name"`
	Columns     []TableColumn `json:"columns"`
	ForeignKeys []ForeignKey  `json:"foreign_keys"`
	Module      string        `json:"module"`
	File        string        `json:"file"`
	IsTenant    bool          `json:"is_tenant"`
	BaseName    string        `json:"base_name,omitempty"`
}

type SQLGraph struct {
	Version   int                 `json:"version"`
	Generated string              `json:"generated"`
	Tables    map[string]TableDef `json:"tables"`
}

// BuildSQLGraph builds a graph of SQL tables with FK extraction and tenant detection.
func BuildSQLGraph(workspaceRoot string, moduleMap map[string][]string) (*SQLGraph, error) {
	graph := &SQLGraph{
		Version:   1,
		Generated: time.Now().Format(time.RFC3339),
		Tables:    make(map[string]TableDef),
	}

	var sqlFiles []string
	err := filepath.WalkDir(workspaceRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if shouldSkipSQLDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".sql") {
			// Ensure it's inside a 'migrations' directory
			// Path processing: path uses filepath.Separator
			pathLower := strings.ToLower(path)
			if strings.Contains(pathLower, "migration") || strings.Contains(pathLower, "schema") {
				sqlFiles = append(sqlFiles, path)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	createTableRe := regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([^\s(]+)\s*\(`)
	columnRe := regexp.MustCompile(`^\s*(\S+)\s+(\S+)(?:\([^)]+\))?\s*(NOT NULL)?\s*(PRIMARY KEY)?\s*(REFERENCES\s+[^\s(]+\s*\([^)]+\))?`)
	fkInlineRe := regexp.MustCompile(`(?i)REFERENCES\s+([^\s(]+)\s*\(([^)]+)\)`)
	fkConstraintRe := regexp.MustCompile(`(?i)FOREIGN\s+KEY\s*\(([^)]+)\)\s*REFERENCES\s*([^\s(]+)\s*\(([^)]+)\)`)

	for _, path := range sqlFiles {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		inCreate := false
		var currentTable *TableDef
		var buffer strings.Builder
		for scanner.Scan() {
			line := scanner.Text()
			if matches := createTableRe.FindStringSubmatch(line); len(matches) > 1 {
				fullName := strings.Trim(matches[1], "`\"'")
				// Skip PostgreSQL format-string artifacts like %I.table_name from dynamic SQL
				if strings.HasPrefix(fullName, "%") || strings.Contains(fullName, "%I") {
					continue
				}
				isTenant := false
				baseName := fullName
				if strings.HasPrefix(fullName, "tenant_") {
					isTenant = true
					if parts := strings.SplitN(fullName, ".", 2); len(parts) == 2 {
						baseName = parts[1]
					} else {
						baseName = strings.TrimPrefix(fullName, "tenant_xxx_")
					}
				}
				currentTable = &TableDef{
					Name:        fullName,
					Columns:     []TableColumn{},
					ForeignKeys: []ForeignKey{},
					File:        path,
					IsTenant:    isTenant,
					BaseName:    baseName,
				}
				inCreate = true
				buffer.Reset()
				continue
			}
			if inCreate && strings.Contains(line, ")") {
				if currentTable != nil {
					for mod, prefixes := range moduleMap {
						for _, prefix := range prefixes {
							if strings.Contains(path, prefix) {
								currentTable.Module = mod
								break
							}
						}
					}
					if currentTable.Module == "" {
						currentTable.Module = GetTableModule(currentTable.Name)
					}
					// Parse FK constraints from buffered content
					content := buffer.String() + "\n" + line
					if fkMatches := fkConstraintRe.FindAllStringSubmatch(content, -1); len(fkMatches) > 0 {
						for _, m := range fkMatches {
							if len(m) == 4 {
								currentTable.ForeignKeys = append(currentTable.ForeignKeys, ForeignKey{
									Column:    strings.TrimSpace(m[1]),
									RefTable:  strings.Trim(m[2], "`\"'"),
									RefColumn: strings.TrimSpace(m[3]),
								})
							}
						}
					}
					graph.Tables[currentTable.Name] = *currentTable
				}
				inCreate = false
				currentTable = nil
				continue
			}
			if inCreate && currentTable != nil {
				buffer.WriteString(line + "\n")
				if colMatch := columnRe.FindStringSubmatch(line); len(colMatch) > 2 {
					col := TableColumn{
						Name: colMatch[1],
						Type: colMatch[2],
					}
					if len(colMatch) > 3 && colMatch[3] != "" {
						col.Nullable = false
					} else {
						col.Nullable = true
					}
					if len(colMatch) > 4 && colMatch[4] != "" {
						col.Key = "PRI"
					}
					currentTable.Columns = append(currentTable.Columns, col)
					// Inline FK references
					if len(colMatch) > 5 && colMatch[5] != "" {
						if fkm := fkInlineRe.FindStringSubmatch(colMatch[5]); len(fkm) == 3 {
							col.Key = "MUL"
							currentTable.Columns[len(currentTable.Columns)-1] = col
							currentTable.ForeignKeys = append(currentTable.ForeignKeys, ForeignKey{
								Column:    col.Name,
								RefTable:  strings.Trim(fkm[1], "`\"'"),
								RefColumn: strings.TrimSpace(fkm[2]),
							})
						}
					}
				}
			}
		}
		file.Close()
	}
	return graph, nil
}

func (g *SQLGraph) Save(path string) error {
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return os.WriteFile(path, data, 0644)
}

func LoadSQLGraph(path string) (*SQLGraph, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var g SQLGraph
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

func shouldSkipSQLDir(name string) bool {
	skip := map[string]bool{
		".git": true, "node_modules": true, "dist": true, "build": true,
	}
	return skip[name]
}

// GetTableModule matches a table name to its corresponding module.
// Module names match the keys in module_map.yaml.
func GetTableModule(tableName string) string {
	name := strings.ToLower(tableName)
	name = strings.TrimPrefix(name, "tenant_")
	// strip schema prefix (e.g. "public." or "xxx.")
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}

	// Audit
	if strings.HasPrefix(name, "audit_") ||
		strings.Contains(name, "txplus") ||
		strings.Contains(name, "tx_plus") ||
		strings.Contains(name, "tx_rules") ||
		strings.Contains(name, "tx_sessions") ||
		name == "audit_events" || name == "audit_checkpoint" {
		return "audit"
	}

	// Accounting / GL
	if strings.HasPrefix(name, "gl_") ||
		strings.HasPrefix(name, "accounting_") ||
		strings.Contains(name, "shareholder") ||
		strings.HasPrefix(name, "journal_") ||
		strings.HasPrefix(name, "bank_") ||
		strings.HasPrefix(name, "cost_centre") ||
		strings.HasPrefix(name, "chart_of_") ||
		strings.HasPrefix(name, "fiscal_") ||
		strings.HasPrefix(name, "deferred_") ||
		strings.HasPrefix(name, "pending_gl") ||
		strings.HasPrefix(name, "item_tax") ||
		strings.Contains(name, "ledger") ||
		strings.Contains(name, "budget") ||
		strings.Contains(name, "fixed_asset") ||
		strings.Contains(name, "commission") ||
		strings.Contains(name, "revenue") {
		return "accounting"
	}

	// Purchasing
	if strings.HasPrefix(name, "purchase_order") ||
		strings.HasPrefix(name, "supplier") ||
		strings.Contains(name, "rfq") ||
		strings.Contains(name, "vendor") ||
		strings.Contains(name, "quality_") ||
		strings.Contains(name, "inspection") ||
		strings.Contains(name, "creditor") ||
		strings.Contains(name, "proforma") ||
		strings.Contains(name, "delivery_note") ||
		strings.Contains(name, "delivery_checklist") ||
		strings.Contains(name, "packing_list") ||
		strings.Contains(name, "donor_programme") ||
		name == "pos" || name == "prs" {
		return "purchasing"
	}

	// Inventory
	if strings.HasPrefix(name, "grn") ||
		strings.HasPrefix(name, "lcv") ||
		strings.HasPrefix(name, "landed_cost") ||
		strings.HasPrefix(name, "kit") ||
		strings.HasPrefix(name, "batch_") ||
		strings.HasPrefix(name, "stock_") ||
		strings.HasPrefix(name, "drug_") ||
		strings.HasPrefix(name, "zone_") ||
		strings.HasPrefix(name, "preferred_location") ||
		strings.HasPrefix(name, "inventory_") ||
		strings.HasPrefix(name, "legacy_stock") ||
		strings.Contains(name, "expiry") ||
		strings.Contains(name, "storage") ||
		name == "drugs" || name == "inventory" || name == "depots" {
		return "inventory"
	}

	// Sales / Dispensing
	if strings.HasPrefix(name, "sale") ||
		strings.HasPrefix(name, "dispensing_") ||
		strings.HasPrefix(name, "prescription") ||
		strings.HasPrefix(name, "patient") ||
		strings.HasPrefix(name, "customer") ||
		strings.HasPrefix(name, "insurance_") ||
		strings.HasPrefix(name, "credit_note") ||
		strings.HasPrefix(name, "etims_") ||
		strings.HasPrefix(name, "prescriber") ||
		strings.Contains(name, "dispense") {
		return "sales"
	}

	return ""
}
package oracode

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type ModuleSummary struct {
	Name        string              `json:"name"`
	Purpose     string              `json:"purpose,omitempty"`
	Warnings    []string            `json:"warnings,omitempty"`
	EntryPoints []RouteEntrySummary `json:"entry_points,omitempty"`
	Tables      []string            `json:"tables,omitempty"`
	RecentEdits []EditEntry         `json:"recent_edits,omitempty"`
	Decisions   []string            `json:"decisions,omitempty"`
}

type RouteEntrySummary struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Handler string `json:"handler"`
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
}

func (s *MCPServer) ModuleSummary(moduleName string) (*ModuleSummary, error) {
	moduleName = strings.TrimSpace(moduleName)
	if moduleName == "" {
		return nil, fmt.Errorf("module required")
	}

	summary := &ModuleSummary{Name: moduleName}
	moduleLower := strings.ToLower(moduleName)

	contractPath := workspaceSourcePath(s.idx.Policy.WorkspaceRoot, "module_contracts", moduleName+".json")
	if data, err := os.ReadFile(contractPath); err == nil {
		var contract struct {
			Purpose  string   `json:"purpose"`
			Warnings []string `json:"warnings"`
		}
		if json.Unmarshal(data, &contract) == nil {
			summary.Purpose = contract.Purpose
			summary.Warnings = append(summary.Warnings, contract.Warnings...)
		}
	}

	if graph, err := s.idx.LoadRouteGraph(); err == nil && graph != nil {
		for _, route := range graph.Routes {
			if !routeBelongsToModule(s, route.File, moduleLower) {
				continue
			}
			summary.EntryPoints = append(summary.EntryPoints, RouteEntrySummary{
				Method:  route.Method,
				Path:    route.Path,
				Handler: firstNonEmpty(route.Resolved, route.Handler),
				File:    route.File,
				Line:    route.Line,
			})
		}
	}

	if sqlGraph, err := LoadSQLGraph(workspaceStatePath(s.idx.Policy.WorkspaceRoot, "sql_graph.json")); err == nil && sqlGraph != nil {
		for _, table := range sqlGraph.Tables {
			if strings.EqualFold(table.Module, moduleName) {
				summary.Tables = append(summary.Tables, table.Name)
			}
		}
	}

	if s.store != nil {
		if edits, err := s.store.RecentEdits(50); err == nil {
			for _, edit := range edits {
				if editMatchesModule(s, edit, moduleLower) {
					summary.RecentEdits = append(summary.RecentEdits, edit)
				}
				if len(summary.RecentEdits) >= 10 {
					break
				}
			}
		}
		if dec, err := s.store.Decisions(400); err == nil {
			for _, line := range strings.Split(dec, "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" {
					continue
				}
				if strings.Contains(strings.ToLower(trimmed), moduleLower) {
					summary.Decisions = append(summary.Decisions, trimmed)
				}
				if len(summary.Decisions) >= 10 {
					break
				}
			}
		}
	}

	return summary, nil
}

func routeBelongsToModule(s *MCPServer, file, moduleLower string) bool {
	if strings.Contains(strings.ToLower(file), moduleLower) {
		return true
	}
	if s != nil && s.store != nil {
		if info, ok, err := s.store.ModuleOwnerForPath(file); err == nil && ok {
			return strings.EqualFold(info.Name, moduleLower)
		}
	}
	return false
}

func editMatchesModule(s *MCPServer, edit EditEntry, moduleLower string) bool {
	for _, file := range edit.Files {
		if routeBelongsToModule(s, file, moduleLower) {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

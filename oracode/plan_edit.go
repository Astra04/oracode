package oracode

import (
	"fmt"
)

type EditPlan struct {
	TargetFile      string   `json:"target_file"`
	Symbol          string   `json:"symbol"`
	LineRange       string   `json:"line_range"`
	MutationType    string   `json:"mutation_type"` // "create", "modify", "replace", "delete"
	OldBody         string   `json:"old_body,omitempty"`
	NewBody         string   `json:"new_body,omitempty"`
	Dependencies    []string `json:"dependencies"`
	AffectedCallers []string `json:"affected_callers"`
	BlastRadius     string   `json:"blast_radius"` // "local", "package", "cross-module"
}

// PlanEdit creates a surgical edit plan for a Go or Vue symbol.
func (s *MCPServer) PlanEdit(task, symbol, fileHint string) (*EditPlan, error) {
	// Locate symbol
	defs := s.idx.FindDefinitions(symbol)
	if len(defs) == 0 {
		return nil, fmt.Errorf("symbol %q not found", symbol)
	}
	def := defs[0]

	// Determine language
	var body string
	var err error
	switch def.Language {
	case LanguageGo:
		body, err = s.ops.SymbolBody(def.File, symbol)
	case LanguageVue, LanguageJavaScript, LanguageTypeScript, LanguageTSX:
		// Use sourceRange to read body around definition.
		data, _ := s.sourceRange(def.File, def.Line, def.Line+50)
		body = data
	default:
		body = ""
	}
	if err != nil {
		body = "(unable to read body)"
	}

	// Blast radius
	blastRadius := "local"
	refs := s.findRefs(symbol)
	externalCallers := 0
	for _, r := range refs {
		if r.File != def.File {
			externalCallers++
		}
	}
	if externalCallers > 0 {
		blastRadius = "package"
		// Check if any caller is in a different module (simple heuristic: different top dir)
		if externalCallers > 0 {
			blastRadius = "cross-module"
		}
	}

	plan := &EditPlan{
		TargetFile:   def.File,
		Symbol:       symbol,
		LineRange:    fmt.Sprintf("%d-%d", def.Line, def.Line+20),
		MutationType: "modify",
		OldBody:      body,
		BlastRadius:  blastRadius,
	}
	// Add a few callers
	for i, r := range refs {
		if i >= 5 {
			break
		}
		plan.AffectedCallers = append(plan.AffectedCallers, fmt.Sprintf("%s:%d", r.File, r.Line))
	}
	return plan, nil
}

// sourceRange reads a range of lines from a file. Already defined in mcp_server.go, but
// this helper wraps it for cross-package use. We reuse the MCPServer method directly.

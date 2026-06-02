package oracode

import (
	"fmt"
)

type EditContext struct {
	Symbol      string           `json:"symbol"`
	File        string           `json:"file"`
	Signature   string           `json:"signature"`
	Body        string           `json:"body"`
	BlastRadius *BlastRadiusNode `json:"blast_radius,omitempty"`
	Module      *ModuleSummary   `json:"module_summary,omitempty"`
	Warnings    []string         `json:"warnings,omitempty"`
}

func (s *MCPServer) PrepareEditContext(file, symbol, moduleName string, depth int) (*EditContext, error) {
	ctx := &EditContext{
		Symbol: symbol,
		File:   file,
	}

	surgOps := NewSurgicalOps(s.idx)

	sig, err := surgOps.SymbolSignature(file, symbol)
	if err == nil {
		ctx.Signature = sig
	} else {
		ctx.Warnings = append(ctx.Warnings, fmt.Sprintf("signature failed: %v", err))
	}

	body, err := surgOps.SymbolBody(file, symbol)
	if err == nil {
		ctx.Body = body
	} else {
		ctx.Warnings = append(ctx.Warnings, fmt.Sprintf("body failed: %v", err))
	}

	if depth <= 0 {
		depth = 2
	}
	radius, err := s.ReverseChain(symbol, depth)
	if err == nil {
		ctx.BlastRadius = radius
	} else {
		ctx.Warnings = append(ctx.Warnings, fmt.Sprintf("blast radius failed: %v", err))
	}

	if moduleName != "" {
		modSum, err := s.ModuleSummary(moduleName)
		if err == nil {
			ctx.Module = modSum
		} else {
			ctx.Warnings = append(ctx.Warnings, fmt.Sprintf("module summary failed: %v", err))
		}
	}

	if ctx.Body == "" && ctx.Signature == "" {
		return nil, fmt.Errorf("could not resolve any context for symbol %q in %s", symbol, file)
	}

	return ctx, nil
}

package oracode

import (
	"fmt"
	"sort"
)

// captureEffectModalities returns a map of symbol -> EffectModality for all symbols
// referenced in the patches (replace_symbol, add_struct_field, remove_struct_field).
func (s *MCPServer) captureEffectModalities(patches []BatchPatch) map[string]*EffectModality {
	result := make(map[string]*EffectModality)
	for _, p := range patches {
		if p.SymbolAnchor != "" && (p.MutationType == "replace_symbol" || p.MutationType == "add_struct_field" || p.MutationType == "remove_struct_field") {
			mod := s.idx.GetEffectModality(p.SymbolAnchor)
			if mod != nil {
				result[p.SymbolAnchor] = mod
			}
		}
	}
	return result
}

// diffEffects compares pre and post effect modality maps and returns a human-readable list of changes.
func (s *MCPServer) diffEffects(pre, post map[string]*EffectModality) []string {
	var changes []string
	allSymbols := make(map[string]bool)
	for sym := range pre {
		allSymbols[sym] = true
	}
	for sym := range post {
		allSymbols[sym] = true
	}

	sorted := make([]string, 0, len(allSymbols))
	for sym := range allSymbols {
		sorted = append(sorted, sym)
	}
	sort.Strings(sorted)

	for _, sym := range sorted {
		preMod, preOk := pre[sym]
		postMod, postOk := post[sym]
		if !preOk && postOk {
			changes = append(changes, fmt.Sprintf("%s: added", sym))
			continue
		}
		if preOk && !postOk {
			changes = append(changes, fmt.Sprintf("%s: removed", sym))
			continue
		}
		if preMod.Error != postMod.Error {
			changes = append(changes, fmt.Sprintf("%s: error %s -> %s", sym, preMod.Error, postMod.Error))
		}
		if preMod.IO != postMod.IO {
			changes = append(changes, fmt.Sprintf("%s: io %s -> %s", sym, preMod.IO, postMod.IO))
		}
		if preMod.Panic != postMod.Panic {
			changes = append(changes, fmt.Sprintf("%s: panic %s -> %s", sym, preMod.Panic, postMod.Panic))
		}
		if preMod.Async != postMod.Async {
			changes = append(changes, fmt.Sprintf("%s: async %s -> %s", sym, preMod.Async, postMod.Async))
		}
		if preMod.Security != postMod.Security {
			changes = append(changes, fmt.Sprintf("%s: security %s -> %s", sym, preMod.Security, postMod.Security))
		}
		if preMod.Resource != postMod.Resource {
			changes = append(changes, fmt.Sprintf("%s: resource %s -> %s", sym, preMod.Resource, postMod.Resource))
		}
	}
	return changes
}
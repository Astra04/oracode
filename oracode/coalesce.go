package oracode

import (
	"fmt"
	"sort"
	"strings"
)

// coalesceFilePatches groups patches per file and applies the three-bucket ordering.
// For each file: symbol patches first, then file lifecycle, then range patches (descending line).
func coalesceFilePatches(patches []BatchPatch) ([]BatchPatch, error) {
	// Group by file
	byFile := make(map[string][]BatchPatch)
	for _, p := range patches {
		byFile[p.File] = append(byFile[p.File], p)
	}

	var result []BatchPatch
	for file, filePatches := range byFile {
		coalesced, err := coalesceFile(file, filePatches)
		if err != nil {
			return nil, err
		}
		result = append(result, coalesced...)
	}
	return result, nil
}

// coalesceFile applies the three-bucket strategy to patches for a single file.
// Bucket 1: symbol patches (replace_symbol, add_struct_field, remove_struct_field)
// Bucket 2: file lifecycle patches (create_file, create_or_patch, rename_file)
// Bucket 3: range_replace patches (sorted descending by start line)
func coalesceFile(file string, patches []BatchPatch) ([]BatchPatch, error) {
	var symbolPatches []BatchPatch
	var lifecyclePatches []BatchPatch
	var rangePatches []BatchPatch

	for _, p := range patches {
		switch p.MutationType {
		case "replace_symbol", "add_struct_field", "remove_struct_field", "replace_symbol_vue", "add_import_vue", "add_composable_vue", "vue_inject_directive", "replace_block":
			symbolPatches = append(symbolPatches, p)
		case "create_file", "create_or_patch", "rename_file":
			lifecyclePatches = append(lifecyclePatches, p)
		case "range_replace":
			rangePatches = append(rangePatches, p)
		default:
			return nil, fmt.Errorf("unknown mutation type: %q", p.MutationType)
		}
	}

	// Merge multiple add_struct_field on the same struct
	mergedSymbols := mergeAddStructFields(symbolPatches)

	// Sort range patches descending by start line (bottom-up to avoid offset drift)
	sort.Slice(rangePatches, func(i, j int) bool {
		return rangePatches[i].StartLine > rangePatches[j].StartLine
	})

	// Concatenate in order: symbolPatches, lifecyclePatches, rangePatches
	out := make([]BatchPatch, 0, len(mergedSymbols)+len(lifecyclePatches)+len(rangePatches))
	out = append(out, mergedSymbols...)
	out = append(out, lifecyclePatches...)
	out = append(out, rangePatches...)
	return out, nil
}

// mergeAddStructFields merges multiple add_struct_field patches on the same struct
// into a single patch with all fields concatenated.
func mergeAddStructFields(patches []BatchPatch) []BatchPatch {
	type key struct {
		file   string
		symbol string
	}
	groups := make(map[key][]BatchPatch)
	var other []BatchPatch

	for _, p := range patches {
		if p.MutationType == "add_struct_field" && p.SymbolAnchor != "" {
			k := key{file: p.File, symbol: p.SymbolAnchor}
			groups[k] = append(groups[k], p)
		} else {
			other = append(other, p)
		}
	}

	merged := make([]BatchPatch, 0, len(other)+len(groups))
	for k, g := range groups {
		var fields []string
		for _, gp := range g {
			fields = append(fields, gp.NewContent)
		}
		merged = append(merged, BatchPatch{
			File:         k.file,
			SymbolAnchor: k.symbol,
			MutationType: "add_struct_field",
			NewContent:   strings.Join(fields, "\n"),
		})
	}
	merged = append(merged, other...)
	return merged
}

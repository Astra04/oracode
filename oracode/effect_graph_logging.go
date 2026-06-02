// effect_graph_logging.go
// Shows how to instrument buildEffectGraph() with logging.
// Merge these patterns into your effect_graph.go / effect.go.

package oracode

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// buildEffectGraphInstrumented shows the full instrumented version of
// buildEffectGraph. Merge this into your Index.buildEffectGraph().
func (idx *Index) buildEffectGraphInstrumented() error {
	graphPath := workspaceStatePath(idx.Policy.WorkspaceRoot, "effect_graph.json")

	// Load previous version to increment.
	prevVersion := 0
	if old, err := LoadEffectGraph(graphPath); err == nil {
		prevVersion = old.Version
	}

	// Start the build span — logs build.start, returns done() for build.done.
	doneBuild := GraphBuildSpan("effect_graph", prevVersion)

	graph := &EffectGraph{
		Version:   prevVersion + 1,
		Generated: time.Now(),
		Symbols:   make(map[string]*EffectModality),
	}

	symbolCount := 0
	skippedFiles := 0

	idx.mu.RLock()
	for rel, meta := range idx.Files {
		if meta.Language != LanguageGo {
			continue
		}

		cst, err := idx.Pool.ParseFile(rel, LanguageGo, idx.Policy)
		if err != nil {
			EffectLog().Warn("effect.parse_error",
				"file", rel,
				"error", err,
			)
			skippedFiles++
			continue
		}

		syms, _ := ExtractSymbols(cst)
		for _, sym := range syms {
			if sym.Kind != "definition" {
				continue
			}
			mod := extractModalitiesFromCST(cst, sym.Name)
			if mod == nil {
				continue
			}
			graph.Symbols[sym.Name] = mod
			symbolCount++

			// Debug: log each extracted symbol with its modalities.
			if log := EffectLog(); log.Enabled(context.Background(), slog.LevelDebug) {
				log.LogAttrs(context.Background(), slog.LevelDebug, "effect.symbol_extracted",
					slog.String("symbol", sym.Name),
					slog.String("file", rel),
					slog.String("io", mod.IO),
					slog.String("error", mod.Error),
					slog.Int("confidence", mod.Confidence),
				)
			}
		}
		cst.Release()
	}
	idx.mu.RUnlock()

	if skippedFiles > 0 {
		EffectLog().Warn("effect.files_skipped",
			"count", skippedFiles,
			"reason", "parse_error",
		)
	}

	if err := graph.Save(graphPath); err != nil {
		doneBuild(prevVersion+1, symbolCount, err)
		return fmt.Errorf("effect graph save failed: %w", err)
	}

	doneBuild(graph.Version, symbolCount, nil)

	// Check memory after graph build — logs only at debug level.
	LogMemory("after_effect_graph_build")

	return nil
}

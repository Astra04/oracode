package oracode

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"
)

// BuildSemanticIndex runs asynchronously with a bounded worker pool to hydrate the vector database.
func (s *MCPServer) BuildSemanticIndex() {
	if s.semanticEngine == nil || s.idx == nil {
		return
	}

	if !s.semanticIndexingActive.CompareAndSwap(false, true) {
		fmt.Fprintf(os.Stderr, "[oracode] BuildSemanticIndex already in progress, skipping concurrent call\n")
		return
	}
	defer s.semanticIndexingActive.Store(false)

	s.idx.mu.RLock()
	symbolsToEmbed := make([]*Symbol, 0)
	for _, symList := range s.idx.Symbols {
		for _, sym := range symList {
			if sym.Kind == "definition" || sym.Kind == "struct" || sym.Kind == "interface" {
				symbolsToEmbed = append(symbolsToEmbed, sym)
			}
		}
	}
	s.idx.mu.RUnlock()

	var upsertedCount int
	var mu sync.Mutex

	// Bounded concurrency to prevent OOM / connection resets on the embedding API
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup

	for _, sym := range symbolsToEmbed {
		id := fmt.Sprintf("%s:%s", sym.File, sym.Name)

		absPath, err := s.idx.Policy.ResolveWorkspacePath(sym.File)
		if err != nil {
			continue
		}
		hash, err := hashFile(absPath)
		if err != nil {
			continue
		}

		s.semanticEngine.mu.RLock()
		existingDoc, exists := s.semanticEngine.Documents[id]
		s.semanticEngine.mu.RUnlock()

		// Skip if staleness check passes
		if exists && existingDoc.FileHash == hash {
			continue
		}

		sem <- struct{}{} // Acquire token (BEFORE wg.Add to prevent hang on panic)
		wg.Add(1)

		go func(sym *Symbol, id string, hash string) {
			defer wg.Done()
			defer func() { <-sem }() // Release token
			// Recover from any panic (e.g. dst@v0.27.3 decorator nil-ptr on
			// unusual Go syntax) so a single bad file can't crash the server.
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(os.Stderr, "[oracode] BuildSemanticIndex: panic on %s: %v\n", id, r)
				}
			}()

			sig, _ := s.ops.SymbolSignature(sym.File, sym.Name)
			body, _ := s.ops.SymbolBody(sym.File, sym.Name)
			if body == "" {
				return
			}

			effectsStr, _ := s.effectModalityShort(sym.Name)

			var callers []string
			refs := s.findRefs(sym.Name)
			for _, ref := range refs {
				enclosing := s.findEnclosingFunction(ref.File, ref.Line)
				if enclosing != "" && enclosing != sym.Name {
					callers = append(callers, enclosing)
				}
			}
			callers = uniqueStrings(callers)

			doc := SemanticDocument{
				ID:        id,
				Symbol:    sym.Name,
				File:      sym.File,
				Signature: sig,
				Effects:   effectsStr,
				Content:   body,
				FileHash:  hash,
				Callers:   callers,
				Type:      "code",
			}

			if err := s.semanticEngine.Upsert(doc); err == nil {
				mu.Lock()
				upsertedCount++
				mu.Unlock()
			} else {
				fmt.Fprintf(os.Stderr, "[oracode] Warning: Upsert failed for %s -> %v\n", id, err)
			}
		}(sym, id, hash)
	}

	// Wait for all workers to finish
	wg.Wait()

	// Batch write to disk if anything was processed
	if upsertedCount > 0 {
		_ = s.semanticEngine.Flush()
		fmt.Fprintf(os.Stderr, "[oracode] Semantic index hydrated with %d updated vectors.\n", upsertedCount)
	}
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

package oracode

import (
	"fmt"
	"go/ast"
	"go/types"
	"os"
	"strings"
	"time"

	"golang.org/x/tools/go/packages"
)

// RunSemanticPass loads full type information and updates the effect graph with high-confidence modalities.
func (idx *Index) RunSemanticPass() error {
	cfg := &packages.Config{
		Mode: packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedImports,
		Dir:  idx.Policy.WorkspaceRoot,
		// Avoid loading test files to reduce memory
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return fmt.Errorf("packages.Load: %w", err)
	}

	for _, pkg := range pkgs {
		if len(pkg.Errors) > 0 {
			// Log but continue with other packages
			fmt.Fprintf(os.Stderr, "[semantic] package %s errors: %v\n", pkg.PkgPath, pkg.Errors)
			continue
		}
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok {
					return true
				}
				symName := fn.Name.Name
				obj := pkg.TypesInfo.Defs[fn.Name]
				if obj == nil {
					return true
				}
				sig, ok := obj.Type().(*types.Signature)
				if !ok {
					return true
				}

				mod := &EffectModality{
					Confidence: 95,
					Error:      "wont",
					IO:         "wont",
					Panic:      "wont",
					Async:      "wont",
					Security:   "wont",
					Resource:   "wont",
				}

				// 1. Error return
				if sig.Results().Len() > 0 {
					last := sig.Results().At(sig.Results().Len() - 1)
					if types.Identical(last.Type(), types.Universe.Lookup("error").Type()) {
						mod.Error = "can"
					}
				}

				// 2. I/O detection
				for i := 0; i < sig.Results().Len(); i++ {
					if isIOType(sig.Results().At(i).Type()) {
						mod.IO = "will"
						break
					}
				}
				for i := 0; i < sig.Params().Len(); i++ {
					if isIOType(sig.Params().At(i).Type()) {
						mod.IO = "will"
						break
					}
				}

				// 3. Resource: any result with Close() method
				for i := 0; i < sig.Results().Len(); i++ {
					if hasCloseMethod(sig.Results().At(i).Type()) {
						mod.Resource = "must"
						break
					}
				}

				// 4. Async: look for go statement in body
				if fn.Body != nil {
					ast.Inspect(fn.Body, func(n ast.Node) bool {
						if _, ok := n.(*ast.GoStmt); ok {
							mod.Async = "might"
							return false
						}
						return true
					})
				}

				// 5. Panic: look for panic call
				if fn.Body != nil {
					ast.Inspect(fn.Body, func(n ast.Node) bool {
						if call, ok := n.(*ast.CallExpr); ok {
							if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "panic" {
								mod.Panic = "will"
								return false
							}
						}
						return true
					})
				}

				// Update effect graph in memory
				idx.effectGraphMu.Lock()
				idx.EffectGraph[symName] = mod
				idx.effectGraphMu.Unlock()

				return true
			})
		}
	}

	// Persist the updated graph
	graphPath := workspaceStatePath(idx.Policy.WorkspaceRoot, "effect_graph.json")
	if err := idx.persistEffectGraph(graphPath); err != nil {
		return err
	}
	return nil
}

// isIOType checks if a type is likely to perform I/O.
func isIOType(t types.Type) bool {
	str := t.String()
	ioPatterns := []string{"io.Reader", "io.Writer", "*os.File", "net.Conn", "os.File"}
	for _, p := range ioPatterns {
		if strings.Contains(str, p) {
			return true
		}
	}
	// Also check if it implements io.Reader or io.Writer (simplified)
	if iface, ok := t.Underlying().(*types.Interface); ok {
		if strings.Contains(iface.String(), "Read") || strings.Contains(iface.String(), "Write") {
			return true
		}
	}
	return false
}

// hasCloseMethod checks if the type has a Close() method.
func hasCloseMethod(t types.Type) bool {
	ptr := types.NewPointer(t)
	obj, _, _ := types.LookupFieldOrMethod(ptr, true, nil, "Close")
	if obj == nil {
		return false
	}
	sig, ok := obj.Type().(*types.Signature)
	if !ok {
		return false
	}
	return sig.Params().Len() == 0 && sig.Results().Len() == 0
}

// persistEffectGraph writes the current effect graph to disk.
func (idx *Index) persistEffectGraph(path string) error {
	idx.effectGraphMu.RLock()
	defer idx.effectGraphMu.RUnlock()
	graph := &EffectGraph{
		Version:   1, // or increment from existing
		Generated: time.Now(),
		Symbols:   idx.EffectGraph,
	}
	return graph.Save(path)
}

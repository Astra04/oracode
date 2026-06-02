package oracode

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dave/dst"
	"github.com/dave/dst/dstutil"
)

// RenameSymbol renames a Go symbol across definitions and references in a scope.
func (s *SurgicalOps) RenameSymbol(oldName, newName, scope string) ([]string, error) {
	if s == nil || s.idx == nil {
		return nil, fmt.Errorf("surgical ops unavailable")
	}
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	scope = strings.TrimSpace(scope)
	if oldName == "" || newName == "" {
		return nil, fmt.Errorf("oldName and newName are required")
	}

	scopeNorm := filepath.ToSlash(filepath.Clean(scope))
	scopeEnabled := scopeNorm != "." && scopeNorm != ""

	changedFiles := make(map[string]struct{})

	defs := s.idx.FindDefinitions(oldName)
	if len(defs) == 0 {
		return nil, fmt.Errorf("symbol %q not found", oldName)
	}
	for _, def := range defs {
		if def == nil {
			continue
		}
		if scopeEnabled && !withinScope(def.File, scopeNorm) {
			continue
		}
		if err := s.renameInFile(def.File, oldName, newName); err != nil {
			return nil, err
		}
		changedFiles[def.File] = struct{}{}
	}

	refs := s.idx.FindReferences(oldName)
	for _, ref := range refs {
		if ref == nil {
			continue
		}
		if scopeEnabled && !withinScope(ref.File, scopeNorm) {
			continue
		}
		if err := s.renameInFile(ref.File, oldName, newName); err != nil {
			return nil, err
		}
		changedFiles[ref.File] = struct{}{}
	}

	var changed []string
	for file := range changedFiles {
		lang, ok := DetectOraLanguage(file)
		if !ok {
			continue
		}
		if err := s.idx.IndexFile(file, lang); err != nil {
			return nil, fmt.Errorf("reindex %s: %w", file, err)
		}
		changed = append(changed, file)
	}
	if len(changed) > 0 {
		_ = s.idx.RefreshRouteGraph()
		_ = s.idx.buildEffectGraph()
	}
	return changed, nil
}

func (s *SurgicalOps) renameInFile(relPath, oldName, newName string) error {
	absPath, err := s.idx.Policy.ResolveWorkspacePath(relPath)
	if err != nil {
		return err
	}
	src, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}
	fileNode, err := SafeDstParse(src)
	if err != nil {
		return err
	}

	modified := false
	dstutil.Apply(fileNode, func(c *dstutil.Cursor) bool {
		ident, ok := c.Node().(*dst.Ident)
		if !ok {
			return true
		}
		if ident.Name == oldName {
			ident.Name = newName
			modified = true
		}
		return true
	}, nil)

	if !modified {
		return nil
	}

	var buf bytes.Buffer
	if err := SafeDstFprint(&buf, fileNode); err != nil {
		return err
	}
	return os.WriteFile(absPath, buf.Bytes(), 0o644)
}

func withinScope(file, scope string) bool {
	fileNorm := filepath.ToSlash(filepath.Clean(file))
	scopeNorm := filepath.ToSlash(filepath.Clean(scope))
	if scopeNorm == "." || scopeNorm == "" {
		return true
	}
	if fileNorm == scopeNorm {
		return true
	}
	if strings.HasPrefix(fileNorm, scopeNorm+"/") {
		return true
	}
	return strings.HasPrefix(fileNorm, scopeNorm)
}

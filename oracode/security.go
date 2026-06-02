package oracode

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const DefaultMaxFileBytes int64 = 2 * 1024 * 1024
const DefaultSQLMaxFileBytes int64 = 2 * 1024 * 1024

type SecurityPolicy struct {
	WorkspaceRoot     string
	MaxFileBytes      int64
	MaxFileBytesByExt map[string]int64
	SkipDirs          map[string]bool
}

func DefaultSecurityPolicy(root string) SecurityPolicy {
	return SecurityPolicy{
		WorkspaceRoot: filepath.Clean(root),
		MaxFileBytes:  DefaultMaxFileBytes,
		MaxFileBytesByExt: map[string]int64{
			".sql": DefaultSQLMaxFileBytes,
		},
		SkipDirs: map[string]bool{
			".git":               true,
			".gocache":           true,
			".gopath":            true,
			"AI edits and plans": true,
			"node_modules":       true,
			"dist":               true,
			"build":              true,
			"coverage":           true,
			// "migrations" intentionally absent — SQL migration files
			// contain schema definitions that must be indexed for
			// scalpel_find_string, scalpel_list_tables, and
			// scalpel_describe_table to work correctly.
			//"scratch":  true,
			//"_scratch": true,
			"tmp":      true,
			"uploads":  true,
		},
	}
}

func (p SecurityPolicy) ResolveWorkspacePath(filePath string) (string, error) {
	root, err := filepath.Abs(p.WorkspaceRoot)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	absPath := filepath.Clean(filepath.Join(root, filePath))
	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		return "", fmt.Errorf("resolve target path: %w", err)
	}
	if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("target escapes workspace: %s", filePath)
	}

	info, err := os.Lstat(absPath)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", filePath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(absPath)
		if err != nil {
			return "", fmt.Errorf("resolve symlink %s: %w", filePath, err)
		}
		resolved = filepath.Clean(resolved)
		rel, err := filepath.Rel(root, resolved)
		if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			return "", fmt.Errorf("symlink target escapes workspace: %s", filePath)
		}
		absPath = resolved
	}

	if !info.IsDir() {
		limit := p.maxFileBytesFor(filePath)
		if info.Size() > limit {
			return "", fmt.Errorf("refusing oversized file %s: %d bytes exceeds %d", filePath, info.Size(), limit)
		}
	}

	return absPath, nil
}

// ResolveWorkspacePathForWrite resolves a workspace path for writing (file may not exist).
// It performs the same traversal and symlink checks as ResolveWorkspacePath but does not
// require the file to exist, and does not check file size limits.
func (p SecurityPolicy) ResolveWorkspacePathForWrite(filePath string) (string, error) {
	root, err := filepath.Abs(p.WorkspaceRoot)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	absPath := filepath.Clean(filepath.Join(root, filePath))
	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		return "", fmt.Errorf("resolve target path: %w", err)
	}
	if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("target escapes workspace: %s", filePath)
	}

	// Check if the path is a symlink (if it exists)
	info, err := os.Lstat(absPath)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(absPath)
		if err != nil {
			return "", fmt.Errorf("resolve symlink %s: %w", filePath, err)
		}
		resolved = filepath.Clean(resolved)
		rel, err := filepath.Rel(root, resolved)
		if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			return "", fmt.Errorf("symlink target escapes workspace: %s", filePath)
		}
		absPath = resolved
	}

	// No file size check because file may not exist yet
	return absPath, nil
}

func (p SecurityPolicy) ShouldSkipDir(name string) bool {
	if p.SkipDirs == nil {
		return false
	}
	return p.SkipDirs[name]
}

func (p SecurityPolicy) maxFileBytesFor(filePath string) int64 {
	ext := strings.ToLower(filepath.Ext(filePath))
	if p.MaxFileBytesByExt != nil {
		if limit, ok := p.MaxFileBytesByExt[ext]; ok && limit > 0 {
			return limit
		}
	}
	if p.MaxFileBytes > 0 {
		return p.MaxFileBytes
	}
	return DefaultMaxFileBytes
}

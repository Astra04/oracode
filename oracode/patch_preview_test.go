package oracode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyPatchPreviewBatchAtomicMultiHunk(t *testing.T) {
	dir := t.TempDir()
	file := "sample.ts"
	fullPath := filepath.Join(dir, file)
	initial := "export const a = 1\nexport const b = 2\nexport const c = 3\n"
	if err := os.WriteFile(fullPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write sample file: %v", err)
	}

	pool, err := NewParserPool()
	if err != nil {
		t.Fatalf("NewParserPool returned error: %v", err)
	}

	server := &MCPServer{idx: &Index{Policy: DefaultSecurityPolicy(dir), Pool: pool}}

	previews, err := server.ApplyPatchPreviewBatch([]PatchHunk{
		{File: file, StartLine: 1, EndLine: 1, NewContent: "export const a = 10"},
		{File: file, StartLine: 3, EndLine: 3, NewContent: "export const c = 30"},
	}, false)
	if err != nil {
		t.Fatalf("ApplyPatchPreviewBatch returned error: %v", err)
	}
	if len(previews) != 2 {
		t.Fatalf("expected 2 previews, got %d", len(previews))
	}

	out, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if !strings.Contains(string(out), "export const a = 1") {
		t.Fatal("file was modified on preview")
	}

	previews, err = server.ApplyPatchPreviewBatch([]PatchHunk{
		{File: file, StartLine: 1, EndLine: 1, NewContent: "export const a = 10"},
		{File: file, StartLine: 3, EndLine: 3, NewContent: "export const c = 30"},
	}, true)
	if err != nil {
		t.Fatalf("ApplyPatchPreviewBatch apply returned error: %v", err)
	}
	if len(previews) != 2 || !previews[0].Applied || !previews[1].Applied {
		t.Fatalf("expected applied previews, got %#v", previews)
	}

	out, err = os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("read file after apply: %v", err)
	}
	if !strings.Contains(string(out), "export const a = 10") || !strings.Contains(string(out), "export const c = 30") {
		t.Fatalf("expected file content updated, got %s", string(out))
	}
}

func TestApplyPatchPreviewSyntaxGuardRejectsInvalidTypeScript(t *testing.T) {
	dir := t.TempDir()
	file := "bad.ts"
	fullPath := filepath.Join(dir, file)
	initial := "export const a = 1\n"
	if err := os.WriteFile(fullPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write sample file: %v", err)
	}

	pool, err := NewParserPool()
	if err != nil {
		t.Fatalf("NewParserPool returned error: %v", err)
	}

	server := &MCPServer{idx: &Index{Policy: DefaultSecurityPolicy(dir), Pool: pool}}
	_, err = server.ApplyPatchPreview(file, 1, 1, "export const a = ;", true)
	if err == nil {
		t.Fatal("expected syntax guard to reject invalid TypeScript")
	}
}

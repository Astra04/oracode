package oracode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveWorkspaceDSNLoadsDevopsEnv(t *testing.T) {
	root := t.TempDir()
	devopsDir := filepath.Join(root, "devops")
	if err := os.MkdirAll(devopsDir, 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	envPath := filepath.Join(devopsDir, ".env")
	envContents := "DATABASE_URL=postgres://user:pass@dbhost:5432/dbname\n"
	if err := os.WriteFile(envPath, []byte(envContents), 0o644); err != nil {
		t.Fatalf("write env failed: %v", err)
	}

	_ = os.Unsetenv("ORACODE_DB_DSN")
	dsn := resolveWorkspaceDSN(filepath.Join(root, "oracode_project"))
	if dsn == "" {
		t.Fatal("expected DSN to be loaded from devops/.env, got empty")
	}
	if !strings.Contains(dsn, "postgres://user:pass@dbhost:5432/dbname") {
		t.Fatalf("unexpected DSN value: %q", dsn)
	}
}

func TestResolveWorkspaceDSNPreservesExistingEnv(t *testing.T) {
	root := t.TempDir()
	if err := os.Setenv("ORACODE_DB_DSN", "postgres://keep:me@host:5432/db"); err != nil {
		t.Fatalf("set env failed: %v", err)
	}
	defer os.Unsetenv("ORACODE_DB_DSN")
	dsn := resolveWorkspaceDSN(root)
	if dsn != "postgres://keep:me@host:5432/db" {
		t.Fatalf("expected existing DSN preserved, got %q", dsn)
	}
}

package oracode

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const stateDirEnv = "ORACODE_STATE_DIR"

func workspaceStateRoot(workspaceRoot string) string {
	root := filepath.Clean(workspaceRoot)

	if override := strings.TrimSpace(os.Getenv(stateDirEnv)); override != "" {
		return filepath.Join(override, workspaceStateKey(root))
	}

	localConfigDir := filepath.Join(root, ".oracode")
	localStateDir := filepath.Join(localConfigDir, "state")
	if info, err := os.Stat(localConfigDir); err == nil && info.IsDir() {
		_ = os.MkdirAll(localStateDir, 0o755)
		return localStateDir
	}

	base, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(base) == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "oracode", workspaceStateKey(root))
}

// ScaffoldWorkspace automatically generates the .oracode skeleton seeded with JamiiLoop DNA.
func ScaffoldWorkspace(workspaceRoot string) error {
	root := filepath.Clean(workspaceRoot)
	baseDir := filepath.Join(root, ".oracode")

	dirs := []string{
		baseDir,
		filepath.Join(baseDir, "state"),
		filepath.Join(baseDir, "workflows"),
		filepath.Join(baseDir, "module_contracts"),
		filepath.Join(baseDir, "templates"),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	jamiiLoopModuleMap := `modules:
  purchasing:
    backend: api/internal/purchasing
    contracts: [creates_grn, raises_po, no_direct_stock_write]
  inventory:
    backend: api/internal/inventory
    contracts: [owns_grn, owns_stock_ledger, fefo_enforced]
  finance:
    backend: api/internal/finance
    contracts: [gl_entry_on_grn, wac_on_receipt, no_direct_balance_write]
  sales:
    backend: api/internal/sales
    contracts: [fefo_dispense, etims_sequential, credit_note_via_cn_module]
  audit:
    backend: api/internal/audit
    contracts: [hash_chained, immutable, append_only, no_updates_no_deletes]
`

	auditHookPattern := `# JamiiLoop Audit Hook Pattern

## Architectural Rules
1. **Application-Layer Hashing:** Hash-chain at the application layer, NOT DB triggers.
2. **Immutability:** The audit log is append-only.
3. **Chaining Mechanism:** Every new audit event must securely reference the SHA-256 hash of the preceding event.
4. **Transaction Boundary:** Execute within the same DB transaction as the primary mutation.
`

	apiTieringPattern := `# JamiiLoop API Tiering Pattern

## Architectural Rules
1. **Middleware Enforcement:** Tiering logic MUST be enforced at the router middleware level, NEVER inside handlers. 
2. **Fail-Closed:** Block the request (403) if a tenant's tier cannot be resolved.
3. **Header/Context:** Downstream handlers should NOT make authorization decisions.
`

	piniaStoreDebug := `# JamiiLoop Frontend Debugging: Pinia & Vue

## Debugging Protocol
1. **Verify the API Contract:** Ensure JSON response matches TypeScript interface exactly.
2. **Trace the Action:** Use 'scalpel_inspect_symbol' to find every Vue component that calls the failing action.
3. **Prop/Emit Boundaries:** Do not mutate props directly.
4. **Reactivity Loss:** Never destructure Pinia state without storeToRefs().
`

	glEntryPattern := `# JamiiLoop General Ledger (GL) Pattern

## Architectural Rules
1. **Double-Entry:** Total Debits == Total Credits. 
2. **No Direct Writes:** Do not UPDATE balance columns directly.
3. **Event-Driven:** Trigger GL entries via Finance API.
4. **WAC:** Re-calculate Inventory value on receipt.
`

	decisionsLog := `# Architectural Decisions Log

- Hash-chain at the application layer (triggers can be disabled per-tenant).
- Tiering is enforced at the router middleware level.
`

	// Empty stub JSON for graph files — populated at runtime by BuildAllGraphs
	emptyGraph := `{"version":1,"generated":"","symbols":{}}`
	var emptySQLGraph = `{"version":1,"generated":"","tables":{}}`
	var emptyRouteGraph = `{"generated_at":"","routes":[]}`
	var emptyProtoGraph = `{"version":1,"generated":"","services":{},"messages":{}}`
	var emptyVueSurface = `{"version":1,"generated":"","components":{}}`
	var emptyFrontendAPI = `{"version":1,"generated":"","calls":[]}`
	emptyConfidenceStore := `{"entries":[]}`
	emptyConstraints := `rules: []`
	emptyWorkflows := `workflows: []`

	files := map[string]string{
		// Workspace DNA
		filepath.Join(baseDir, "decisions.md"):          decisionsLog,
		filepath.Join(baseDir, "module_map.yaml"):       jamiiLoopModuleMap,
		filepath.Join(baseDir, "edit_log.jsonl"):        "",
		filepath.Join(baseDir, ".env"):                  "# JamiiLoop workspace overrides\n",
		filepath.Join(baseDir, "config.json"):           `{"version":1}`,
		filepath.Join(baseDir, "constraints.yaml"):      emptyConstraints,
		filepath.Join(baseDir, "workflows.yaml"):        emptyWorkflows,
		filepath.Join(baseDir, "confidence_store.json"): emptyConfidenceStore,

		// Graph stubs — rebuilt at runtime
		filepath.Join(baseDir, "effect_graph.json"): emptyGraph,
		filepath.Join(baseDir, "sql_graph.json"):    emptySQLGraph,
		filepath.Join(baseDir, "route_graph.json"):  emptyRouteGraph,
		filepath.Join(baseDir, "proto_graph.json"):  emptyProtoGraph,
		filepath.Join(baseDir, "vue_surface.json"):  emptyVueSurface,
		filepath.Join(baseDir, "frontend_api.json"): emptyFrontendAPI,

		// Workflow templates
		filepath.Join(baseDir, "workflows", "audit_hook_pattern.md"):  auditHookPattern,
		filepath.Join(baseDir, "workflows", "api_tiering_pattern.md"): apiTieringPattern,
		filepath.Join(baseDir, "workflows", "pinia_store_debug.md"):   piniaStoreDebug,
		filepath.Join(baseDir, "workflows", "gl_entry_pattern.md"):    glEntryPattern,
	}

	for path, template := range files {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			_ = os.WriteFile(path, []byte(template), 0o644)
		}
	}
	return nil
}

func workspaceStateKey(workspaceRoot string) string {
	sum := sha1.Sum([]byte(filepath.Clean(workspaceRoot)))
	return hex.EncodeToString(sum[:])
}

func workspaceStatePath(workspaceRoot string, parts ...string) string {
	elems := append([]string{workspaceStateRoot(workspaceRoot)}, parts...)
	return filepath.Join(elems...)
}

func workspaceSourcePath(workspaceRoot string, parts ...string) string {
	elems := append([]string{filepath.Clean(workspaceRoot), ".oracode"}, parts...)
	return filepath.Join(elems...)
}

func ensureStateSeed(workspaceRoot string, statePath string, sourceParts ...string) error {
	if _, err := os.Stat(statePath); err == nil {
		return nil
	}
	source := workspaceSourcePath(workspaceRoot, sourceParts...)
	data, err := os.ReadFile(source)
	if err == nil {
		if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
			return fmt.Errorf("create state dir: %w", err)
		}
		if err := os.WriteFile(statePath, data, 0o644); err != nil {
			return fmt.Errorf("seed state file %s: %w", statePath, err)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return nil
	}
	return nil
}

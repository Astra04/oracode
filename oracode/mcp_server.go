package oracode

import (
	"net/http"
	"net/url"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MCP JSON-RPC 2.0 types
type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *mcpError   `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"inputSchema"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpCallResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

// MCPServer
type MCPServer struct {
	idx                    *Index
	store                  *EditStore
	confStore              *ConfidenceStore
	ops                    *SurgicalOps
	semanticEngine         *SemanticEngine
	reader                 *bufio.Scanner
	writer                 io.Writer
	initDone               chan struct{}
	initOnce               sync.Once
	semanticIndexingActive atomic.Bool
}

func NewMCPServer(idx *Index) *MCPServer {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	var store *EditStore
	var confStore *ConfidenceStore
	var semanticEngine *SemanticEngine
	if idx != nil {
		// Auto-scaffold the .oracode/ directory to force local state resolution.
		// This prevents the silent fallback to %LOCALAPPDATA% which breaks the
		// "roaming agent" architecture required for a project-specific knowledge base.
		workspaceRoot := idx.Policy.WorkspaceRoot
		if workspaceRoot != "" {
			if err := ScaffoldWorkspace(workspaceRoot); err != nil {
				fmt.Fprintf(os.Stderr, "[oracode] warning: failed to scaffold workspace %s: %v\n", workspaceRoot, err)
			}
		}

		s, err := NewEditStore(workspaceRoot)
		if err == nil {
			store = s
		}
		cs, err := NewConfidenceStore(workspaceRoot)
		if err == nil {
			confStore = cs
		}
		semanticEngine = NewSemanticEngine(workspaceRoot)
	}
	return &MCPServer{idx: idx, store: store, confStore: confStore, ops: NewSurgicalOps(idx), semanticEngine: semanticEngine, reader: scanner, writer: os.Stdout, initDone: make(chan struct{})}
}

func (s *MCPServer) Close() {
	if s.semanticEngine != nil {
		s.semanticEngine.Close()
	}
}

// lazyInit triggers a background reindex on the first tool call.
// It returns immediately — callers receive stale-but-real cached data while
// the background goroutine walks the workspace and rebuilds all indices.
// The stale warning injected in handleToolCall tells the LLM to re-query
// once reindexing is done.
func (s *MCPServer) lazyInit() {
	s.initOnce.Do(func() {
		// Signal ready immediately so the first tool call is never blocked.
		// The GOB cache (loaded in NewIndex / NewSemanticEngine) already has
		// last-known data from the previous run.
		close(s.initDone)

		if s.idx == nil {
			return
		}

		// Decide whether a fresh walk is needed.
		s.idx.mu.RLock()
		alreadyIndexed := len(s.idx.Files) > 0
		s.idx.mu.RUnlock()

		if alreadyIndexed {
			// Cache was loaded — run a silent background refresh to catch
			// any edits made while the server was down.
			s.idx.IndexingActive.Store(true)
			go func() {
				defer s.idx.IndexingActive.Store(false)
				fmt.Fprintf(os.Stderr, "[oracode] background refresh started (cache loaded, %d files)\n", len(s.idx.Files))
				if err := s.idx.WalkDirectory(); err != nil {
					fmt.Fprintf(os.Stderr, "[oracode] background walk error: %v\n", err)
				}
				fmt.Fprintf(os.Stderr, "[oracode] background refresh complete\n")
				// ❌ Semantic index build removed from startup to prevent CPU starvation
				// that causes MCP timeout disconnects. Triggered on-demand via
				// scalpel_reindex or scalpel_semantic_search.
			}()
		} else {
			// No cache — do the first-ever walk in the background.
			s.idx.IndexingActive.Store(true)
			go func() {
				defer s.idx.IndexingActive.Store(false)
				fmt.Fprintf(os.Stderr, "[oracode] initial workspace index started (no cache found)\n")
				if err := s.idx.WalkDirectory(); err != nil {
					fmt.Fprintf(os.Stderr, "[oracode] walk error: %v\n", err)
				}
				fmt.Fprintf(os.Stderr, "[oracode] initial indexing complete\n")
				if s.semanticEngine != nil {
					semanticDocCount := func() int {
						s.semanticEngine.mu.RLock()
						defer s.semanticEngine.mu.RUnlock()
						return len(s.semanticEngine.Documents)
					}()
					if semanticDocCount == 0 {
						fmt.Fprintf(os.Stderr, "[oracode] vector store empty — building deferred. Trigger via scalpel_reindex or scalpel_semantic_search.\n")
					} else {
						fmt.Fprintf(os.Stderr, "[oracode] vector store loaded (%d docs) — hydration deferred. Trigger via scalpel_reindex or scalpel_semantic_search.\n", semanticDocCount)
					}
					// ❌ BuildSemanticIndex removed from startup. On-demand only.
				}
			}()
		}
	})
	<-s.initDone
}

func (s *MCPServer) Run() error {
	for s.reader.Scan() {
		line := s.reader.Text()
		if line == "" {
			continue
		}
		var req mcpRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		resp := s.handle(req)
		if resp.JSONRPC == "" && resp.ID == nil && resp.Result == nil && resp.Error == nil {
			continue
		}
		out, _ := json.Marshal(resp)
		fmt.Fprintf(s.writer, "%s\n", out)
	}
	return s.reader.Err()
}

func (s *MCPServer) handle(req mcpRequest) mcpResponse {
	switch req.Method {
	case "initialize":
		return mcpResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
				"serverInfo":      map[string]string{"name": "oracode-scalpel", "version": "1.1.0"},
			},
		}

	case "notifications/initialized":
		return mcpResponse{}

	case "tools/list":
		tools := []mcpTool{

			// ==================== WATCHDOG INTEGRATION ====================
			{
				Name:        "watchdog_errors",
				Description: "Fetch runtime errors captured by the watchdog system from state.json",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"service": map[string]string{"type": "string", "description": "Optional: 'Main API', 'TX+', 'PocketBase', 'Frontend', 'system'"},
						"limit":   map[string]interface{}{"type": "integer", "description": "Max results (default 20)"},
					},
				},
			},
			{
				Name:        "watchdog_state",
				Description: "Get full system state: CPU, RAM, Disk, PG, service health, tenant telemetry",
				InputSchema: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
			{
				Name:        "watchdog_supervisor",
				Description: "Control a supervised service (start, stop, restart)",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"service": map[string]string{"type": "string", "description": "Required: 'Main API', 'TX+', 'PocketBase', 'Frontend'"},
						"action":  map[string]string{"type": "string", "description": "Required: 'start', 'stop', 'restart'"},
					},
					"required": []string{"service", "action"},
				},
			},
			{
				Name:        "watchdog_logs",
				Description: "Get recent log lines from a supervised service",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"service": map[string]string{"type": "string", "description": "Required: 'Main API', 'TX+', 'PocketBase', 'Frontend'"},
						"lines":   map[string]interface{}{"type": "integer", "description": "Optional: default 200, max 2000"},
					},
					"required": []string{"service"},
				},
			},

			// ==================== PHASE 1: DISCOVERY & ONBOARDING ====================
			{
				Name:        "scalpel_context_packet",
				Description: "When you start a new task in an unfamiliar module, get a dense payload with the module's contract, effect modalities, SQL tables, HTTP routes, recent decisions, and architectural constraints in one call.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"task":   map[string]string{"type": "string", "description": "Short description of the task (used as a label)"},
						"module": map[string]string{"type": "string", "description": "Module name to assemble context for"},
					},
					"required": []string{"task", "module"},
				},
			},
			{
				Name:        "scalpel_inspect_route",
				Description: "Trace an HTTP endpoint from the route registration, through middleware, down to the handler and SQL tables. Modes: list all routes, trace a URL, or full call chain.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"mode":   map[string]string{"type": "string", "description": "Operation mode: 'list' (show all routes), 'trace' (show middleware+handler), 'chain' (full call graph)"},
						"url":    map[string]string{"type": "string", "description": "URL path to trace (required for mode=trace or chain)"},
						"method": map[string]string{"type": "string", "description": "HTTP method (GET, POST, etc.) - optional, matches any if omitted"},
						"path":   map[string]string{"type": "string", "description": "Directory path filter for listing routes"},
						"depth":  map[string]string{"type": "integer", "description": "Depth of call chain (default 3)"},
						"limit":  map[string]string{"type": "integer", "description": "Max number of routes to return (default 200)"},
					},
				},
			},
			{
				Name:        "scalpel_surface",
				Description: "Extract structured schema for SQL tables, Protobuf services, or Vue components without searching raw files.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"kind": map[string]string{"type": "string", "description": "Type of surface: 'sql', 'proto', or 'vue' (default 'sql')"},
						"name": map[string]string{"type": "string", "description": "Module name (for sql/vue) or service name (for proto)"},
					},
					"required": []string{"name"},
				},
			},
			{
				Name:        "scalpel_semantic_search",
				Description: "PHASE 1 (Discovery): You are navigating a large OSS project or translating critical workloads to a lower-level language (e.g., Go to Rust/C). You don't know the exact symbol names, but you know the *intent* (e.g., 'where do we handle concurrent rate limiting'). This tool performs a hybrid search (BM25 keyword + Semantic Cosine Similarity via Comet) and returns the most relevant code blocks along with their effect modalities and caller contexts. Crucial for cross-language translation where understanding side-effects and caller intent is more important than exact syntax.",
				InputSchema: map[string]interface{}{
					"type":     "object",
					"required": []string{"query"},
					"properties": map[string]interface{}{
						"query":          map[string]string{"type": "string", "description": "Conceptual search query (e.g., 'JWT validation middleware with Redis cache')"},
						"limit":          map[string]string{"type": "integer", "description": "Number of results to return (default 5)"},
						"include_scores": map[string]string{"type": "boolean", "description": "Return exact Vector, BM25, and Fused RRF scores for metacognitive analysis of the results."},
					},
				},
			},
			{
				Name:        "scalpel_find_string",
				Description: "Search for a string or substring across all indexed files (faster than grep, no shell).",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query":         map[string]string{"type": "string", "description": "Text to search for"},
						"file":          map[string]string{"type": "string", "description": "Restrict search to one file"},
						"lang":          map[string]string{"type": "string", "description": "Restrict search to language (go, vue, ts, sql)"},
						"caseSensitive": map[string]string{"type": "boolean", "description": "Case-sensitive search (default false)"},
						"wholeWord":     map[string]string{"type": "boolean", "description": "Match whole words only (default false)"},
						"limit":         map[string]string{"type": "integer", "description": "Max results (default 50, max 500)"},
					},
					"required": []string{"query"},
				},
			},
			{
				Name:        "scalpel_list_routes",
				Description: "List all HTTP route registrations found in Go files (supports gin, chi, echo, net/http).",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path":  map[string]string{"type": "string", "description": "Directory prefix to filter routes by file path"},
						"limit": map[string]string{"type": "integer", "description": "Max routes to return (default 200)"},
					},
				},
			},
			{
				Name:        "scalpel_list_tables",
				Description: "List every SQL table definition indexed in the workspace (migrations, clean SQL, backups).",
				InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
			},
			{
				Name:        "scalpel_describe_table",
				Description: "Show the columns, types, and constraints of a specific SQL table from its CREATE TABLE definition.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"table": map[string]string{"type": "string", "description": "Table name (case-insensitive)"},
						"limit": map[string]string{"type": "integer", "description": "Max matching definitions to return (default 20)"},
					},
					"required": []string{"table"},
				},
			},
			{
				Name:        "scalpel_get_file_symbols",
				Description: "List all indexed definitions and references for a given file.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file": map[string]string{"type": "string", "description": "Relative file path"},
					},
					"required": []string{"file"},
				},
			},
			{
				Name:        "scalpel_get_range",
				Description: "Read a precise line range from a file with line numbers (max 240 lines).",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file":  map[string]string{"type": "string", "description": "Relative file path"},
						"start": map[string]string{"type": "integer", "description": "Start line (1-indexed)"},
						"end":   map[string]string{"type": "integer", "description": "End line (defaults to start)"},
					},
					"required": []string{"file", "start"},
				},
			},
			{
				Name:        "scalpel_find_symbol",
				Description: "Find all definitions of a symbol (function, struct, variable, type) across the workspace.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": map[string]string{"type": "string", "description": "Symbol name to find"},
						"lang": map[string]string{"type": "string", "description": "Filter by language (go, vue, ts, sql)"},
						"kind": map[string]string{"type": "string", "description": "Filter by kind (definition, reference)"},
					},
					"required": []string{"name"},
				},
			},
			{
				Name:        "scalpel_find_refs",
				Description: "Find all call sites and usages of a symbol.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": map[string]string{"type": "string", "description": "Symbol name to find references for"},
					},
					"required": []string{"name"},
				},
			},
			{
				Name:        "scalpel_get_symbol_context",
				Description: "Return source lines around a symbol's definition with optional references.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name":         map[string]string{"type": "string", "description": "Symbol name"},
						"contextLines": map[string]string{"type": "integer", "description": "Lines of context above/below (default 8, max 40)"},
						"includeRefs":  map[string]string{"type": "boolean", "description": "Also list all reference sites"},
					},
					"required": []string{"name"},
				},
			},
			{
				Name:        "scalpel_read_symbol",
				Description: "Read only the body of a function or definition without loading the whole file.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": map[string]string{"type": "string", "description": "Symbol name"},
					},
					"required": []string{"name"},
				},
			},
			{
				Name:        "scalpel_dependencies",
				Description: "List all symbols referenced by a file or by a specific symbol.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file":   map[string]string{"type": "string", "description": "File path (if symbol omitted)"},
						"symbol": map[string]string{"type": "string", "description": "Symbol name (resolves to its file)"},
					},
				},
			},
			{
				Name:        "scalpel_list_imports",
				Description: "List all import paths used by a file (deduplicated).",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file": map[string]string{"type": "string", "description": "Relative file path"},
					},
					"required": []string{"file"},
				},
			},
			{
				Name:        "scalpel_module_owner",
				Description: "Resolve which module owns a given file path using module_map.yaml.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file": map[string]string{"type": "string", "description": "Relative file path"},
					},
					"required": []string{"file"},
				},
			},
			{
				Name:        "scalpel_git_changes",
				Description: "List files changed in git since a given reference (default HEAD~1).",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"since": map[string]string{"type": "string", "description": "Git revision (e.g., HEAD~1, main, abc123)"},
					},
				},
			},
			{
				Name:        "scalpel_reindex",
				Description: "Reindex a specific file or the entire workspace after edits. Call this after making code changes so other tools see the updated symbols.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file": map[string]string{"type": "string", "description": "Relative file path (omit to reindex whole workspace)"},
					},
				},
			},

			// ==================== PHASE 2: ANALYSIS & PLANNING ====================
			{
				Name:        "scalpel_prepare_edit_context",
				Description: "Before modifying a symbol, get its source code (signature + body), reverse call graph (blast radius up to depth 3), and any module-specific constraints.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file":   map[string]string{"type": "string", "description": "File containing the symbol"},
						"symbol": map[string]string{"type": "string", "description": "Symbol name to edit"},
						"module": map[string]string{"type": "string", "description": "Optional module name to attach module summary"},
					},
					"required": []string{"file", "symbol"},
				},
			},
			{
				Name:        "scalpel_effect",
				Description: "Analyse a function to determine its effect modalities (error, I/O, panic, async, security, resource) so you know if calling it is safe.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"mode":  map[string]string{"type": "string", "description": "Operation mode: 'modality' (single symbol), 'trace' (propagation), 'version' (graph metadata)"},
						"name":  map[string]string{"type": "string", "description": "Symbol name (required for modality/trace)"},
						"depth": map[string]string{"type": "integer", "description": "Trace depth (default 3)"},
					},
				},
			},
			{
				Name:        "scalpel_inspect_symbol",
				Description: "Unified symbol lookup: get definitions, references, context, or raw body in one call.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name":         map[string]string{"type": "string", "description": "Symbol name"},
						"mode":         map[string]string{"type": "string", "description": "What to return: 'definition', 'refs', 'context', 'body' (default context)"},
						"contextLines": map[string]string{"type": "integer", "description": "Lines around definition (for context mode)"},
						"includeRefs":  map[string]string{"type": "boolean", "description": "Include references in context mode"},
					},
					"required": []string{"name"},
				},
			},
			{
				Name:        "scalpel_blast_radius",
				Description: "Show the dependents and impacts of a symbol as a reverse call chain tree.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"symbol": map[string]string{"type": "string", "description": "Symbol name"},
						"depth":  map[string]string{"type": "integer", "description": "How many hops to trace (default 4)"},
					},
					"required": []string{"symbol"},
				},
			},
			{
				Name:        "scalpel_trace_effects",
				Description: "Trace effect propagation up the call chain from a symbol (shows every caller's modalities).",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name":  map[string]string{"type": "string", "description": "Starting symbol"},
						"depth": map[string]string{"type": "integer", "description": "How many hops to trace (default 3)"},
					},
					"required": []string{"name"},
				},
			},
			{
				Name:        "scalpel_graph_version",
				Description: "Return the version number and build timestamp of the current effect graph. Use to check if the graph is stale.",
				InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
			},
			{
				Name:        "scalpel_effect_modality",
				Description: "Return effect modalities (error, io, panic, async, security, resource) for a single Go symbol.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": map[string]string{"type": "string", "description": "Symbol name"},
					},
					"required": []string{"name"},
				},
			},
			{
				Name:        "scalpel_module_summary",
				Description: "Return a one-page summary for a module: purpose, routes, tables, edits, and decisions.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"module": map[string]string{"type": "string", "description": "Module name"},
					},
					"required": []string{"module"},
				},
			},
			{
				Name:        "scalpel_module_contracts",
				Description: "Load and view the contract specification for a given module (expected effect modalities and wiring rules).",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"module": map[string]string{"type": "string", "description": "Module name"},
					},
					"required": []string{"module"},
				},
			},
			{
				Name:        "scalpel_find_violations",
				Description: "Compare the module's contract against the actual effect graph; reports every symbol where actual behaviour deviates.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"module": map[string]string{"type": "string", "description": "Module name"},
					},
					"required": []string{"module"},
				},
			},
			{
				Name:        "scalpel_schema_surface",
				Description: "Return SQL schema surface (tables, columns, types) for a module.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"module": map[string]string{"type": "string", "description": "Module name"},
					},
					"required": []string{"module"},
				},
			},
			{
				Name:        "scalpel_proto_surface",
				Description: "Return protobuf surface (methods, request/response types) for a gRPC service.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"service": map[string]string{"type": "string", "description": "gRPC service name"},
					},
					"required": []string{"service"},
				},
			},
			{
				Name:        "scalpel_vue_surface",
				Description: "Return Vue SFC risk/binding surface for a module (components, composables, missing cleanup).",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"module": map[string]string{"type": "string", "description": "Module name"},
					},
					"required": []string{"module"},
				},
			},

			// ==================== PHASE 3: SURGICAL EXECUTION ====================
			{
				Name:        "scalpel_batch_edit",
				Description: "Apply multiple file changes atomically. Supports Vue ops (replace_symbol_vue, add_import_vue, add_composable_vue, vue_inject_directive, replace_block) and Go ops (replace_symbol, add_struct_field, range_replace, create_file). For range_replace in Vue, specifying a block (e.g. 'template') interprets lines relative to that block.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"patches":                     map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object"}, "description": "List of patches (file, group, mutation_type, block, start_line, end_line, new_content, symbol_anchor, tag, match_attr, directive)"},
						"dry_run":                     map[string]string{"type": "boolean", "description": "Only preview, don't write"},
						"atomic":                      map[string]string{"type": "boolean", "description": "Rollback all groups if any fails"},
						"stop_on_first_group_failure": map[string]string{"type": "boolean", "description": "Stop processing groups after first failure"},
						"read_before":                 map[string]string{"type": "boolean", "description": "Log original file content before changes"},
						"trace_before":                map[string]string{"type": "boolean", "description": "Capture effect modalities before edit"},
						"check_effects":               map[string]string{"type": "boolean", "description": "Compare effect modalities before/after"},
						"validate_after":              map[string]string{"type": "boolean", "description": "Run architectural constraints after edit"},
						"record_edit":                 map[string]string{"type": "boolean", "description": "Save edit to edit_log.jsonl"},
						"cleanup_imports":             map[string]string{"type": "boolean", "description": "Remove unused imports from Go files"},
						"validate_wiring":             map[string]string{"type": "boolean", "description": "Check wiring constraints"},
					},
					"required": []string{"patches"},
				},
			},
			{
				Name:        "scalpel_patch_symbol_body",
				Description: "Replace a specific substring inside a function body, validates the new AST, and writes back only if syntactically correct.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file":       map[string]string{"type": "string", "description": "Relative file path"},
						"symbol":     map[string]string{"type": "string", "description": "Function name to modify"},
						"search":     map[string]string{"type": "string", "description": "Exact string block to find inside the body"},
						"replace":    map[string]string{"type": "string", "description": "Replacement block"},
						"confidence": map[string]string{"type": "integer", "description": "0-100 rating of how confident you are (recorded in confidence store)"},
					},
					"required": []string{"file", "symbol", "search", "replace"},
				},
			},
			{
				Name:        "scalpel_scaffold",
				Description: "Generate full feature skeletons (controllers, services, Vue components) from team templates via a JSON manifest.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"manifest": map[string]string{"type": "string", "description": "Path to scaffold manifest JSON"},
						"dry_run":  map[string]string{"type": "boolean", "description": "Only preview, don't write"},
					},
					"required": []string{"manifest"},
				},
			},
			{
				Name:        "scalpel_replace_symbol",
				Description: "Replace a Go declaration (function, type, var, const) with a supplied declaration snippet.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file":        map[string]string{"type": "string", "description": "Relative file path"},
						"name":        map[string]string{"type": "string", "description": "Symbol name to replace"},
						"replacement": map[string]string{"type": "string", "description": "Full new declaration snippet"},
					},
					"required": []string{"file", "name", "replacement"},
				},
			},
			{
				Name:        "scalpel_add_struct_field",
				Description: "Append a Go struct field declaration to a named struct.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file":   map[string]string{"type": "string", "description": "Relative file path"},
						"struct": map[string]string{"type": "string", "description": "Struct name"},
						"field":  map[string]string{"type": "string", "description": "Field declaration (e.g., 'ID int `json:\"id\"`')"},
					},
					"required": []string{"file", "struct", "field"},
				},
			},
			{
				Name:        "scalpel_remove_struct_field",
				Description: "Remove a named field from a Go struct.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file":   map[string]string{"type": "string", "description": "Relative file path"},
						"struct": map[string]string{"type": "string", "description": "Struct name"},
						"field":  map[string]string{"type": "string", "description": "Field name"},
					},
					"required": []string{"file", "struct", "field"},
				},
			},
			{
				Name:        "scalpel_rename_symbol",
				Description: "Rename a Go symbol across definitions and references within a scope.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"old_name": map[string]string{"type": "string", "description": "Current symbol name"},
						"new_name": map[string]string{"type": "string", "description": "New symbol name"},
						"scope":    map[string]string{"type": "string", "description": "Directory or file to restrict rename (e.g., '.' or 'internal/users')"},
					},
					"required": []string{"old_name", "new_name", "scope"},
				},
			},
			{
				Name:        "scalpel_replace_symbol_vue",
				Description: "Replace a Vue/TypeScript symbol (function, const, let) inside a Vue SFC script block using tree-sitter.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file":        map[string]string{"type": "string", "description": "Relative path to .vue file"},
						"symbol":      map[string]string{"type": "string", "description": "Symbol name to replace"},
						"replacement": map[string]string{"type": "string", "description": "New source for that symbol"},
					},
					"required": []string{"file", "symbol", "replacement"},
				},
			},
			{
				Name:        "scalpel_add_import_vue",
				Description: "Add an ES import statement to a Vue SFC script block.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file":        map[string]string{"type": "string", "description": "Relative path to .vue file"},
						"import_path": map[string]string{"type": "string", "description": "Import path (e.g., 'lodash' or '{ ref } from 'vue'')"},
					},
					"required": []string{"file", "import_path"},
				},
			},
			{
				Name:        "scalpel_add_composable_vue",
				Description: "Inject a composable call into the setup() function or <script setup> block of a Vue SFC.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file": map[string]string{"type": "string", "description": "Relative path to .vue file"},
						"call": map[string]string{"type": "string", "description": "Composable call expression, e.g. 'useMyFeature()'"},
					},
					"required": []string{"file", "call"},
				},
			},
			{
				Name:        "scalpel_sfc_replace_block",
				Description: "Replace only the script, template, or style block in a Vue SFC. Preserves whitespace and indentation.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file":    map[string]string{"type": "string", "description": "Relative path to .vue file"},
						"block":   map[string]string{"type": "string", "description": "Which block to replace (script, template, style)"},
						"content": map[string]string{"type": "string", "description": "New content for the block"},
					},
					"required": []string{"file", "block", "content"},
				},
			},
			{
				Name:        "scalpel_sfc_read_block",
				Description: "Read a specific block (script, template, style) of a Vue SFC.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file":  map[string]string{"type": "string", "description": "Relative path to .vue file"},
						"block": map[string]string{"type": "string", "description": "Block type: script, template, or style"},
					},
					"required": []string{"file", "block"},
				},
			},
			{
				Name:        "scalpel_apply_pattern",
				Description: "Apply a capture-based structural pattern replacement inside a scope. Supports optional 'block' for Vue SFC offset translation.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"lang":        map[string]string{"type": "string", "description": "Language (go, vue, ts, etc.)"},
						"pattern":     map[string]string{"type": "string", "description": "Regex with capture groups (e.g., `fmt\\.Errorf\\(([^)]+)\\)`)"},
						"replacement": map[string]string{"type": "string", "description": "Replacement with $1, $2"},
						"scope":       map[string]string{"type": "string", "description": "Directory or file to apply to"},
						"block":       map[string]string{"type": "string", "description": "Vue block name (script, template, style) for offset-aware replacement"},
						"dry_run":     map[string]string{"type": "boolean", "description": "Only report matches, don't write"},
					},
					"required": []string{"lang", "pattern", "replacement", "scope"},
				},
			},
			{
				Name:        "scalpel_apply_patch_preview",
				Description: "Preview unified diff of a range replacement, then optionally apply. Use symbol_anchor to avoid line drift.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file":          map[string]string{"type": "string", "description": "Relative file path"},
						"symbol_anchor": map[string]string{"type": "string", "description": "Symbol name to resolve lines dynamically"},
						"start_line":    map[string]string{"type": "integer", "description": "Start line (1-indexed)"},
						"end_line":      map[string]string{"type": "integer", "description": "End line"},
						"new_content":   map[string]string{"type": "string", "description": "New content to replace the range"},
						"apply":         map[string]string{"type": "boolean", "description": "If true, write the change"},
					},
					"required": []string{"file", "new_content"},
				},
			},
			{
				Name:        "scalpel_plan_edit",
				Description: "Generate a surgical edit plan: target file, symbol, blast radius, and affected callers.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"task":   map[string]string{"type": "string", "description": "Natural language description of the change"},
						"symbol": map[string]string{"type": "string", "description": "Symbol to edit"},
						"file":   map[string]string{"type": "string", "description": "Optional file hint"},
					},
				},
			},
			{
				Name:        "scalpel_record_edit",
				Description: "Write a structured edit record to edit_log.jsonl. Call this after every change so future agents have context.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"task":         map[string]string{"type": "string", "description": "What you were doing"},
						"reason":       map[string]string{"type": "string", "description": "Why you made the change"},
						"files":        map[string]interface{}{"type": "array", "items": map[string]string{"type": "string"}, "description": "Files changed"},
						"symbols":      map[string]interface{}{"type": "array", "items": map[string]string{"type": "string"}, "description": "Key symbols touched"},
						"verification": map[string]string{"type": "string", "description": "How you verified the change"},
						"decision":     map[string]string{"type": "string", "description": "Any architecture decision made"},
						"pattern":      map[string]string{"type": "string", "description": "Effect pattern learned"},
						"confidence":   map[string]string{"type": "integer", "description": "Confidence score 0-100"},
					},
					"required": []string{"task", "files"},
				},
			},
			{
				Name:        "scalpel_edit_log",
				Description: "Query edit history. filter: 'all', 'file:<path>', or 'symbol:<name>'. Replaces recent_edits, edits_for_file, edits_for_symbol.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"filter": map[string]string{"type": "string", "description": "all | file:<path> | symbol:<name>"},
						"limit":  map[string]string{"type": "integer", "description": "Max results (default 10)"},
					},
				},
			},
			{
				Name:        "scalpel_recent_edits",
				Description: "Return the most recent recorded edits across the whole workspace.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"limit": map[string]string{"type": "integer", "description": "Number of edits to return (default 10, max 100)"},
					},
				},
			},
			{
				Name:        "scalpel_edits_for_file",
				Description: "Return recorded edits that touched a specific file.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file":  map[string]string{"type": "string", "description": "Relative file path"},
						"limit": map[string]string{"type": "integer", "description": "Max results (default 10)"},
					},
					"required": []string{"file"},
				},
			},
			{
				Name:        "scalpel_edits_for_symbol",
				Description: "Return recorded edits that mention a specific symbol.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name":  map[string]string{"type": "string", "description": "Symbol name"},
						"limit": map[string]string{"type": "integer", "description": "Max results (default 10)"},
					},
					"required": []string{"name"},
				},
			},
			{
				Name:        "scalpel_decision_log",
				Description: "Return the latest architecture decisions from decisions.md.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"limit": map[string]string{"type": "integer", "description": "Max lines to return (default 80, max 400)"},
					},
				},
			},
			{
				Name:        "scalpel_log_decision",
				Description: "Append an architectural decision and reasoning to decisions.md so future agents understand the change. MANDATORY before non-trivial edits.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"module":    map[string]string{"type": "string", "description": "Module affected"},
						"decision":  map[string]string{"type": "string", "description": "The decision made"},
						"reasoning": map[string]string{"type": "string", "description": "Why this decision was chosen"},
					},
					"required": []string{"module", "decision", "reasoning"},
				},
			},
			{
				Name:        "scalpel_read_team_workflow",
				Description: "Read team SOPs from Markdown files in .oracode/workflows/ to align your coding style with project conventions.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"workflow_name": map[string]string{"type": "string", "description": "Workflow file name (e.g., 'add_endpoint.md') - omit to list all"},
					},
				},
			},

			// ==================== PHASE 4: DEBUGGING & VALIDATION ====================
			{
				Name:        "scalpel_verify_with_context",
				Description: "Run 'go build' and return the first compiler error with the exact failing line and 5 lines of surrounding code (marked with '>').",
				InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
			},
			{
				Name:        "scalpel_validate",
				Description: "Check code against architectural constraints, module contracts, or policy files. Modes: 'code' (snippet), 'contract' (module vs effect graph), 'policy' (YAML rules).",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"mode":     map[string]string{"type": "string", "description": "Operation mode: 'code', 'contract', or 'policy' (default 'code')"},
						"code":     map[string]string{"type": "string", "description": "Source code snippet (required for mode=code)"},
						"language": map[string]string{"type": "string", "description": "Language of the snippet (required for mode=code)"},
						"module":   map[string]string{"type": "string", "description": "Module name (required for mode=contract)"},
						"policy":   map[string]string{"type": "string", "description": "Path to policy YAML (required for mode=policy)"},
					},
				},
			},
			{
				Name:        "scalpel_validate_code",
				Description: "Run all AST-based and regex architectural constraints on a code snippet.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"code":     map[string]string{"type": "string", "description": "Source code to validate"},
						"language": map[string]string{"type": "string", "description": "Language (go, vue, ts, sql)"},
					},
					"required": []string{"code", "language"},
				},
			},
			{
				Name:        "scalpel_run_policy",
				Description: "Run AST/pattern policy constraints against Go files in the workspace.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"policy": map[string]string{"type": "string", "description": "Path to policy YAML file"},
					},
					"required": []string{"policy"},
				},
			},
			{
				Name:        "scalpel_trace_error",
				Description: "Trace a watchdog error ref into the endpoint, call chain, and schema context.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"ref": map[string]string{"type": "string", "description": "Error reference (e.g., 'ERR_AUTH_502')"},
					},
					"required": []string{"ref"},
				},
			},
			{
				Name:        "scalpel_watch_errors",
				Description: "Tail an error log file, match stack traces to symbols in the index.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"log":   map[string]string{"type": "string", "description": "Path to log file"},
						"limit": map[string]string{"type": "integer", "description": "Max errors to return (default 20)"},
					},
					"required": []string{"log"},
				},
			},
			{
				Name:        "scalpel_schema_gap",
				Description: "Compare migration SQL (sql_graph.json) against a live PostgreSQL schema via ORACODE_DB_DSN.",
				InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
			},
			{
				Name:        "scalpel_schema_breadcrumbs",
				Description: "Show foreign key relationships and tenant scope for a SQL table.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"table": map[string]string{"type": "string", "description": "Table name"},
					},
					"required": []string{"table"},
				},
			},
			{
				Name:        "scalpel_frontend_api_calls",
				Description: "List API calls (axios/fetch) made in Vue components, optionally filtered by component name.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"component": map[string]string{"type": "string", "description": "Filter by component name (optional)"},
					},
				},
			},
			{
				Name:        "scalpel_trace_chain",
				Description: "Trace a full call chain from an HTTP route down through Go calls and SQL tables.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"url":    map[string]string{"type": "string", "description": "URL path"},
						"method": map[string]string{"type": "string", "description": "HTTP method (default ANY)"},
						"depth":  map[string]string{"type": "integer", "description": "Trace depth (default 4)"},
					},
					"required": []string{"url"},
				},
			},
			{
				Name:        "scalpel_trace_request",
				Description: "Trace middleware chain and handler for a URL path (shallow trace).",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"url":    map[string]string{"type": "string", "description": "URL path"},
						"method": map[string]string{"type": "string", "description": "HTTP method (default ANY)"},
						"limit":  map[string]string{"type": "integer", "description": "Max matches (default 20)"},
						"depth":  map[string]string{"type": "integer", "description": "Call chain depth (default 1)"},
					},
					"required": []string{"url"},
				},
			},
			{
				Name:        "scalpel_vue_inject_directive",
				Description: "You are implementing frontend tiering, dynamic approval logic, or feature flags. You need to add a structural directive (like v-if=\"hasTier('enterprise')\") to specific elements across Vue files. Standard text replacement is dangerous and breaks Vue syntax. This tool safely parses the <template> block, finds the exact element (e.g., 'q-btn' with 'action=\"approve\"'), and structurally injects the directive without breaking formatting.",
				InputSchema: map[string]interface{}{
					"type":     "object",
					"required": []string{"file", "tag", "directive"},
					"properties": map[string]interface{}{
						"file":       map[string]string{"type": "string", "description": "Path to the .vue file"},
						"tag":        map[string]string{"type": "string", "description": "The HTML/Vue tag to target (e.g., 'q-btn', 'div', 'ApproveCard')"},
						"match_attr": map[string]string{"type": "string", "description": "Optional attribute snippet that must exist on the tag to ensure we hit the right one (e.g., 'label=\"Approve\"' or '@click=\"submit\"')"},
						"directive":  map[string]string{"type": "string", "description": "The exact Vue directive to inject (e.g., 'v-if=\"requiresApproval\"')"},
						"dry_run":    map[string]string{"type": "boolean", "description": "If true, returns what would change without writing to disk"},
					},
				},
			},
			{
				Name:        "scalpel_evaluate_pattern",
				Description: "Validate a code snippet against architectural constraints and find existing similar implementations in the codebase. Use during brainstorming to avoid hallucinating bad designs.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"snippet":  map[string]string{"type": "string", "description": "Proposed code snippet to evaluate"},
						"language": map[string]string{"type": "string", "description": "Language (go, vue, ts, sql)"},
						"scope":    map[string]string{"type": "string", "description": "Optional directory path to restrict the search"},
					},
					"required": []string{"snippet", "language"},
				},
			},
		}
		return mcpResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]interface{}{"tools": tools},
		}

	case "tools/call":
		return s.handleToolCall(req)

	default:
		return mcpResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &mcpError{Code: -32601, Message: "method not found: " + req.Method},
		}
	}
}

func (s *MCPServer) handleToolCall(req mcpRequest) (resp mcpResponse) {
	s.lazyInit()

	var params struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return errResp(req.ID, "invalid params")
	}

	var keyArgs []any
	if name, ok := params.Arguments["name"].(string); ok {
		keyArgs = append(keyArgs, "symbol", name)
	}
	if file, ok := params.Arguments["file"].(string); ok {
		keyArgs = append(keyArgs, "file", file)
	}

	done := ToolSpan(context.Background(), params.Name, keyArgs...)
	defer func() {
		var resultBytes int
		var err error
		if resp.Error != nil {
			err = fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
		} else if callRes, ok := resp.Result.(mcpCallResult); ok {
			if callRes.IsError {
				var errMsg string
				if len(callRes.Content) > 0 {
					errMsg = callRes.Content[0].Text
				}
				err = fmt.Errorf("%s", errMsg)
			} else {
				for _, c := range callRes.Content {
					resultBytes += len(c.Text)
				}
			}
		}
		done(resultBytes, err)
	}()

	resp = s.dispatchToolCall(req, params.Name, params.Arguments)

	// Inject a stale-data warning banner when background reindexing is still
	// running. This tells the model the data may be outdated and it can
	// re-query the same tool once reindexing completes.
	astStale := s.idx != nil && s.idx.IndexingActive.Load()
	vecStale := s.semanticEngine != nil && (s.semanticEngine.WarmingUp.Load() || s.semanticIndexingActive.Load())
	if (astStale || vecStale) && resp.Error == nil {
		if callRes, ok := resp.Result.(mcpCallResult); ok && !callRes.IsError && len(callRes.Content) > 0 {
			what := "AST symbol index"
			if astStale && vecStale {
				what = "AST symbol index and vector store"
			} else if vecStale {
				what = "vector/semantic index"
			}
			banner := fmt.Sprintf(
				"> [!WARNING]\n"+
					"> **Oracode background reindexing in progress** — the %s is still warming up.\n"+
					"> Data returned below is from the **last cached state** and may not reflect recent edits.\n"+
					"> Once reindexing completes you are free to call this tool again for fully up-to-date results.\n\n",
				what,
			)
			callRes.Content[0].Text = banner + callRes.Content[0].Text
			resp.Result = callRes
		}
	}

	return resp
}

// dispatchToolCall contains the actual switch-case routing for tool calls.
func (s *MCPServer) dispatchToolCall(req mcpRequest, toolName string, arguments map[string]interface{}) mcpResponse {
	params := struct {
		Name      string
		Arguments map[string]interface{}
	}{Name: toolName, Arguments: arguments}

	switch toolName {

	// ===== PHASE 1: DISCOVERY & ONBOARDING =====
	case "scalpel_find_symbol":
		name, _ := params.Arguments["name"].(string)
		langFilter, _ := params.Arguments["lang"].(string)
		kindFilter, _ := params.Arguments["kind"].(string)
		if name == "" {
			return errResp(req.ID, "name is required")
		}
		syms := s.idx.FindDefinitions(name)
		if len(syms) == 0 {
			return textResp(req.ID, fmt.Sprintf("No symbol found: %q", name))
		}
		out := fmt.Sprintf("## Definitions for `%s`\n", name)
		count := 0
		for _, sym := range syms {
			if langFilter != "" && string(sym.Language) != langFilter {
				continue
			}
			if kindFilter != "" && sym.Kind != kindFilter {
				continue
			}
			out += fmt.Sprintf("- `%s` kind=%s lang=%s → %s:%d\n", sym.Name, sym.Kind, sym.Language, sym.File, sym.Line)
			count++
		}
		if count == 0 {
			return textResp(req.ID, fmt.Sprintf("No symbol found for filters lang=%q kind=%q", langFilter, kindFilter))
		}
		return textResp(req.ID, out)

	case "scalpel_find_refs":
		name, _ := params.Arguments["name"].(string)
		if name == "" {
			return errResp(req.ID, "name is required")
		}
		refs := s.findRefs(name)
		if len(refs) == 0 {
			return textResp(req.ID, fmt.Sprintf("No references found for `%s`.", name))
		}
		out := fmt.Sprintf("## References to `%s` (%d sites)\n", name, len(refs))
		for _, r := range refs {
			out += fmt.Sprintf("- %s:%d (lang=%s)\n", r.File, r.Line, r.Language)
		}
		return textResp(req.ID, out)

	case "scalpel_get_symbol_context":
		name, _ := params.Arguments["name"].(string)
		if name == "" {
			return errResp(req.ID, "name is required")
		}
		contextLines := intArg(params.Arguments, "contextLines", 8)
		if contextLines < 0 {
			contextLines = 0
		}
		if contextLines > 40 {
			contextLines = 40
		}
		includeRefs, _ := params.Arguments["includeRefs"].(bool)
		text, err := s.symbolContext(name, contextLines, includeRefs)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		return textResp(req.ID, text)

	case "scalpel_get_range":
		file, _ := params.Arguments["file"].(string)
		if file == "" {
			return errResp(req.ID, "file is required")
		}
		start := intArg(params.Arguments, "start", 1)
		end := intArg(params.Arguments, "end", start)
		text, err := s.sourceRange(file, start, end)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		return textResp(req.ID, text)

	case "scalpel_get_file_symbols":
		file, _ := params.Arguments["file"].(string)
		if file == "" {
			return errResp(req.ID, "file is required")
		}
		return textResp(req.ID, s.fileSymbols(file))

	case "scalpel_dependencies":
		file, _ := params.Arguments["file"].(string)
		symbol, _ := params.Arguments["symbol"].(string)
		text, err := s.dependencies(file, symbol)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		return textResp(req.ID, text)

	case "scalpel_list_imports":
		file, _ := params.Arguments["file"].(string)
		if file == "" {
			return errResp(req.ID, "file is required")
		}
		syms := s.lookupFileSymbols(file)
		var imports []string
		for _, sym := range syms {
			if sym.Kind == "module" {
				imports = sym.Imports
				break
			}
		}
		if len(imports) == 0 {
			return textResp(req.ID, fmt.Sprintf("No imports found in %s", file))
		}
		imports = uniqueStrings(imports)
		out := fmt.Sprintf("## Imports in `%s`\n", file)
		for _, imp := range imports {
			out += fmt.Sprintf("- `%s`\n", imp)
		}
		return textResp(req.ID, out)

	case "scalpel_list_tables":
		s.idx.mu.RLock()
		var tables []*Symbol
		for _, symList := range s.idx.Symbols {
			for _, sym := range symList {
				if sym.Language == LanguageSQL &&
					(sym.Kind == "definition" || sym.Kind == "table" || sym.Kind == "CREATE") {
					tables = append(tables, sym)
				}
			}
		}
		s.idx.mu.RUnlock()
		if len(tables) == 0 {
			return textResp(req.ID, "No SQL tables indexed.")
		}
		out := fmt.Sprintf("## SQL Tables (%d)\n", len(tables))
		for _, t := range tables {
			out += fmt.Sprintf("- `%s` → %s:%d\n", t.Name, t.File, t.Line)
		}
		return textResp(req.ID, out)

	case "scalpel_reindex":
		file, _ := params.Arguments["file"].(string)
		if file != "" {
			lang, ok := DetectOraLanguage(file)
			if !ok {
				return errResp(req.ID, "unsupported file type: "+file)
			}
			if err := s.idx.IndexFile(file, lang); err != nil {
				return errResp(req.ID, err.Error())
			}
			_ = s.idx.RefreshRouteGraph()
			return textResp(req.ID, fmt.Sprintf("Re-indexed: %s", file))
		}
		if err := s.idx.WalkDirectory(); err != nil {
			return errResp(req.ID, err.Error())
		}
		s.idx.mu.RLock()
		totalSyms := len(s.idx.Symbols)
		totalFiles := len(s.idx.Files)
		s.idx.mu.RUnlock()

		// Trigger semantic index build in the background so the MCP connection
		// is never blocked by CPU-bound embedding generation. The server returns
		// immediately; stale-flagged results will be served until warmup completes.
		if s.semanticEngine != nil {
			go func() {
				fmt.Fprintf(os.Stderr, "[oracode] semantic index build triggered by reindex\n")
				s.BuildSemanticIndex()
			}()
		}

		return textResp(req.ID, fmt.Sprintf("Full reindex complete. Files: %d, Symbols: %d", totalFiles, totalSyms))

	case "scalpel_list_files":
		pathFilter, _ := params.Arguments["path"].(string)
		pattern, _ := params.Arguments["pattern"].(string)
		limit := intArg(params.Arguments, "limit", 100)
		if limit < 1 {
			limit = 100
		}
		if limit > 1000 {
			limit = 1000
		}
		text, err := s.listFiles(pathFilter, pattern, limit)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		return textResp(req.ID, text)

	case "scalpel_find_string":
		query, _ := params.Arguments["query"].(string)
		if strings.TrimSpace(query) == "" {
			return errResp(req.ID, "query is required")
		}
		file, _ := params.Arguments["file"].(string)
		langFilter, _ := params.Arguments["lang"].(string)
		caseSensitive, _ := params.Arguments["caseSensitive"].(bool)
		limit := intArg(params.Arguments, "limit", 50)
		if limit < 1 {
			limit = 50
		}
		if limit > 500 {
			limit = 500
		}
		text, err := s.findString(query, file, langFilter, caseSensitive, limit)
		if err != nil {
			return errResp(req.ID, err.Error())
		}

		if strings.Contains(text, "No matches found for") && file == "" && langFilter == "" {
			// Fallback to semantic search so the tool call doesn't go to waste
			if s.semanticEngine != nil {
				semResults, semErr := s.semanticEngine.HybridSearch(query, limit, "code")
				if semErr == nil && len(semResults) > 0 {
					var out strings.Builder
					fmt.Fprintf(&out, "## No exact string matches found. Falling back to Semantic Search Results:\n\n")
					for _, doc := range semResults {
						fmt.Fprintf(&out, "### `%s`\n**File**: %s\n**Score**: %.2f\n```go\n%s\n```\n\n", doc.Symbol, doc.File, doc.FusedScore, doc.Signature)
					}
					return textResp(req.ID, out.String())
				}
			}
		}

		return textResp(req.ID, text)

	case "scalpel_list_routes":
		pathFilter, _ := params.Arguments["path"].(string)
		limit := intArg(params.Arguments, "limit", 200)
		if limit < 1 {
			limit = 200
		}
		if limit > 2000 {
			limit = 2000
		}
		text, err := s.listRoutes(pathFilter, limit)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		return textResp(req.ID, text)

	case "scalpel_trace_request":
		url, _ := params.Arguments["url"].(string)
		if strings.TrimSpace(url) == "" {
			return errResp(req.ID, "url is required")
		}
		method, _ := params.Arguments["method"].(string)
		depth := intArg(params.Arguments, "depth", 1)
		if depth < 1 {
			depth = 1
		}
		if depth > 5 {
			depth = 5
		}
		limit := intArg(params.Arguments, "limit", 20)
		if limit < 1 {
			limit = 20
		}
		if limit > 200 {
			limit = 200
		}
		text, err := s.traceRequest(url, method, limit, depth)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		return textResp(req.ID, text)

	case "scalpel_trace_chain":
		url, _ := params.Arguments["url"].(string)
		if strings.TrimSpace(url) == "" {
			return errResp(req.ID, "url is required")
		}
		method, _ := params.Arguments["method"].(string)
		depth := intArg(params.Arguments, "depth", 4)
		if depth < 1 {
			depth = 1
		}
		if depth > 8 {
			depth = 8
		}
		text, err := s.traceChain(url, method, depth)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		return textResp(req.ID, text)

	case "scalpel_rename_symbol":
		oldName, _ := params.Arguments["old_name"].(string)
		newName, _ := params.Arguments["new_name"].(string)
		scope, _ := params.Arguments["scope"].(string)
		if strings.TrimSpace(oldName) == "" || strings.TrimSpace(newName) == "" || strings.TrimSpace(scope) == "" {
			return errResp(req.ID, "old_name, new_name, scope required")
		}
		if s.ops == nil {
			return errResp(req.ID, "surgical ops unavailable")
		}
		changed, err := s.ops.RenameSymbol(oldName, newName, scope)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		if len(changed) == 0 {
			return textResp(req.ID, "No files changed.")
		}
		return textResp(req.ID, fmt.Sprintf("Renamed %q → %q in %d files:\n%s", oldName, newName, len(changed), strings.Join(changed, "\n")))

	case "scalpel_schema_gap":
		gap, err := s.SchemaGap()
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		out, _ := json.MarshalIndent(gap, "", "  ")
		return textResp(req.ID, "## Schema Gap\n```json\n"+string(out)+"\n```")

	// ===== WATCHDOG INTEGRATION =====
	case "watchdog_errors":
		service, _ := params.Arguments["service"].(string)
		limitF, ok := params.Arguments["limit"].(float64)
		limit := 20
		if ok { limit = int(limitF) }

		res, err := s.fetchWatchdogArtifact("errors.jsonl") // Or state.json if we parse it
		if err != nil { return errResp(req.ID, err.Error()) }

		// Very simple filter: if service is provided, we just string match for now.
		// Alternatively, we return state.json and let the user filter.
		// For robustness, let's just return the raw or lightly processed artifacts.
		if service != "" {
			lines := strings.Split(res, "\n")
			var filtered []string
			for _, line := range lines {
				if strings.Contains(line, service) {
					filtered = append(filtered, line)
				}
			}
			if len(filtered) > limit { filtered = filtered[:limit] }
			return textResp(req.ID, strings.Join(filtered, "\n"))
		}

		lines := strings.Split(res, "\n")
		if len(lines) > limit { lines = lines[:limit] }
		return textResp(req.ID, strings.Join(lines, "\n"))

	case "watchdog_state":
		res, err := s.fetchWatchdogArtifact("state.json")
		if err != nil { return errResp(req.ID, err.Error()) }
		return textResp(req.ID, res)

	case "watchdog_supervisor":
		service, _ := params.Arguments["service"].(string)
		action, _ := params.Arguments["action"].(string)
		if service == "" || action == "" { return errResp(req.ID, "service and action required") }

		res, err := s.postWatchdogSupervisor(service, action)
		if err != nil { return errResp(req.ID, err.Error()) }
		return textResp(req.ID, res)

	case "watchdog_logs":
		service, _ := params.Arguments["service"].(string)
		linesF, ok := params.Arguments["lines"].(float64)
		lines := 200
		if ok { lines = int(linesF) }
		if service == "" { return errResp(req.ID, "service required") }

		res, err := s.fetchWatchdogLogs(service, lines)
		if err != nil { return errResp(req.ID, err.Error()) }
		return textResp(req.ID, res)

	case "scalpel_trace_error":
		ref, _ := params.Arguments["ref"].(string)
		if strings.TrimSpace(ref) == "" {
			return errResp(req.ID, "ref required")
		}
		report, err := s.TraceError(ref)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		out, _ := json.MarshalIndent(report, "", "  ")
		return textResp(req.ID, "## Error Report\n```json\n"+string(out)+"\n```")

	case "scalpel_module_summary":
		module, _ := params.Arguments["module"].(string)
		if strings.TrimSpace(module) == "" {
			return errResp(req.ID, "module required")
		}
		summary, err := s.ModuleSummary(module)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		out, _ := json.MarshalIndent(summary, "", "  ")
		return textResp(req.ID, "## Module Summary\n```json\n"+string(out)+"\n```")

	case "scalpel_blast_radius":
		symbol, _ := params.Arguments["symbol"].(string)
		depth := intArg(params.Arguments, "depth", 4)
		if strings.TrimSpace(symbol) == "" {
			return errResp(req.ID, "symbol required")
		}
		root, err := s.ReverseChain(symbol, depth)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		return textResp(req.ID, "## Blast Radius\n```text\n"+formatBlastTree(root, "")+"```")

	case "scalpel_apply_pattern":
		lang, _ := params.Arguments["lang"].(string)
		pattern, _ := params.Arguments["pattern"].(string)
		replacement, _ := params.Arguments["replacement"].(string)
		scope, _ := params.Arguments["scope"].(string)
		block, _ := params.Arguments["block"].(string)
		dryRun, _ := params.Arguments["dry_run"].(bool)
		if lang == "" || pattern == "" || replacement == "" || scope == "" {
			return errResp(req.ID, "lang, pattern, replacement, scope required")
		}
		var matches []PatternMatch
		var patternErr error
		info, statErr := os.Stat(scope)
		if statErr == nil && !info.IsDir() && lang == "vue" && block != "" {
			matches, patternErr = s.ApplyStructuralPattern(scope, lang, pattern, replacement, block, dryRun)
		} else {
			matches, patternErr = s.idx.ApplyPattern(lang, pattern, replacement, scope, dryRun)
		}
		if patternErr != nil {
			return errResp(req.ID, patternErr.Error())
		}
		out, _ := json.MarshalIndent(matches, "", "  ")
		return textResp(req.ID, fmt.Sprintf("## Pattern Matches (dry run=%v)\n```json\n%s\n```", dryRun, out))

	case "scalpel_run_policy":
		policyPath, _ := params.Arguments["policy"].(string)
		if policyPath == "" {
			return errResp(req.ID, "policy path required")
		}
		violations, err := s.idx.RunPolicy(policyPath)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		if len(violations) == 0 {
			return textResp(req.ID, "Policy passed – no violations found.")
		}
		return textResp(req.ID, "## Policy Violations\n"+strings.Join(violations, "\n"))

	case "scalpel_watch_errors":
		logPath, _ := params.Arguments["log"].(string)
		limit := intArg(params.Arguments, "limit", 20)
		if logPath == "" {
			return errResp(req.ID, "log path required")
		}
		matches, err := s.idx.ScanErrors(logPath, limit)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		if len(matches) == 0 {
			return textResp(req.ID, "No error matches found.")
		}
		out := "## Error Matches\n"
		for _, m := range matches {
			out += fmt.Sprintf("- %s:%d | symbol %q | line: %s\n", m.File, m.LineNum, m.Symbol, m.Line)
		}
		return textResp(req.ID, out)

	case "scalpel_describe_table":
		table, _ := params.Arguments["table"].(string)
		if strings.TrimSpace(table) == "" {
			return errResp(req.ID, "table is required")
		}
		limit := intArg(params.Arguments, "limit", 20)
		if limit < 1 {
			limit = 20
		}
		if limit > 100 {
			limit = 100
		}
		text, err := s.describeTable(table, limit)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		return textResp(req.ID, text)

	case "scalpel_recent_edits":
		if s.store == nil {
			return errResp(req.ID, "edit store not available")
		}
		limit := intArg(params.Arguments, "limit", 10)
		if limit < 1 {
			limit = 10
		}
		if limit > 100 {
			limit = 100
		}
		edits, err := s.store.RecentEdits(limit)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		return textResp(req.ID, renderEditEntries("Recent Edits", edits))

	case "scalpel_edits_for_file":
		if s.store == nil {
			return errResp(req.ID, "edit store not available")
		}
		file, _ := params.Arguments["file"].(string)
		if file == "" {
			return errResp(req.ID, "file is required")
		}
		limit := intArg(params.Arguments, "limit", 10)
		if limit < 1 {
			limit = 10
		}
		if limit > 100 {
			limit = 100
		}
		edits, err := s.store.EditsForFile(file, limit)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		return textResp(req.ID, renderEditEntries(fmt.Sprintf("Edits for %s", file), edits))

	case "scalpel_edits_for_symbol":
		if s.store == nil {
			return errResp(req.ID, "edit store not available")
		}
		name, _ := params.Arguments["name"].(string)
		if name == "" {
			return errResp(req.ID, "name is required")
		}
		limit := intArg(params.Arguments, "limit", 10)
		if limit < 1 {
			limit = 10
		}
		if limit > 100 {
			limit = 100
		}
		edits, err := s.store.EditsForSymbol(name, limit)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		return textResp(req.ID, renderEditEntries(fmt.Sprintf("Edits for symbol %s", name), edits))

	case "scalpel_record_edit":
		if s.store == nil {
			return errResp(req.ID, "edit store not available")
		}
		entry, err := parseEditEntryArgs(params.Arguments)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		if err := s.store.RecordEdit(entry); err != nil {
			return errResp(req.ID, err.Error())
		}
		if s.confStore != nil {
			pattern, _ := params.Arguments["pattern"].(string)
			if pattern != "" {
				conf := intArg(params.Arguments, "confidence", 100)
				s.confStore.Record(pattern, conf)
			}
		}
		return textResp(req.ID, "Edit recorded.")

	case "scalpel_decision_log":
		if s.store == nil {
			return errResp(req.ID, "edit store not available")
		}
		limit := intArg(params.Arguments, "limit", 80)
		if limit < 1 {
			limit = 1
		}
		if limit > 400 {
			limit = 400
		}
		text, err := s.store.Decisions(limit)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		if strings.TrimSpace(text) == "" {
			text = "No decisions logged yet."
		}
		return textResp(req.ID, text)

	case "scalpel_module_owner":
		if s.store == nil {
			return errResp(req.ID, "edit store not available")
		}
		file, _ := params.Arguments["file"].(string)
		if file == "" {
			return errResp(req.ID, "file is required")
		}
		info, ok, err := s.store.ModuleOwnerForPath(file)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		if !ok {
			return textResp(req.ID, fmt.Sprintf("No module owner mapping matched `%s`.", file))
		}
		return textResp(req.ID, renderModuleInfo(file, info))

	// ===== PHASE 2: ANALYSIS & PLANNING =====
	case "scalpel_prepare_edit_context":
		file, _ := params.Arguments["file"].(string)
		symbol, _ := params.Arguments["symbol"].(string)
		module, _ := params.Arguments["module"].(string)
		depth := intArg(params.Arguments, "depth", 3)
		if file == "" || symbol == "" {
			return errResp(req.ID, "file and symbol required")
		}
		ctx, err := s.PrepareEditContext(file, symbol, module, depth)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		out, _ := json.MarshalIndent(ctx, "", "  ")
		return textResp(req.ID, "## Edit Context\n```json\n"+string(out)+"\n```")

	case "scalpel_effect_modality":
		name, _ := params.Arguments["name"].(string)
		if name == "" {
			return errResp(req.ID, "name required")
		}
		graph, err := s.idx.GetEffectGraph()
		if err != nil {
			return errResp(req.ID, fmt.Sprintf("effect graph error: %v", err))
		}
		if graph == nil {
			return textResp(req.ID, "Effect graph not built yet. Run reindex.")
		}
		mod, ok := graph.Symbols[name]
		if !ok {
			return textResp(req.ID, fmt.Sprintf("No effect data for symbol %q", name))
		}
		out := fmt.Sprintf("## Effect Modalities for `%s`\n", name)
		out += fmt.Sprintf("- error: %s\n", mod.Error)
		out += fmt.Sprintf("- io: %s\n", mod.IO)
		out += fmt.Sprintf("- panic: %s\n", mod.Panic)
		out += fmt.Sprintf("- async: %s\n", mod.Async)
		out += fmt.Sprintf("- security: %s\n", mod.Security)
		out += fmt.Sprintf("- resource: %s\n", mod.Resource)
		out += fmt.Sprintf("- confidence: %d%%\n", mod.Confidence)
		out += fmt.Sprintf("\nGraph version: %d, generated: %s\n", graph.Version, graph.Generated.Format(time.RFC3339))
		return textResp(req.ID, out)

	case "scalpel_graph_version":
		graph, err := s.idx.GetEffectGraph()
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		if graph == nil {
			return textResp(req.ID, "No effect graph built yet.")
		}
		return textResp(req.ID, fmt.Sprintf("effect_graph version %d (generated %s)", graph.Version, graph.Generated.Format(time.RFC3339)))

	case "scalpel_trace_effects":
		startSym, _ := params.Arguments["name"].(string)
		depth := intArg(params.Arguments, "depth", 3)
		if startSym == "" {
			return errResp(req.ID, "name required")
		}
		text, err := s.traceEffects(startSym, depth)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		return textResp(req.ID, text)

	case "scalpel_read_symbol":
		name, _ := params.Arguments["name"].(string)
		if name == "" {
			return errResp(req.ID, "name required")
		}
		defs := s.idx.FindDefinitions(name)
		if len(defs) == 0 {
			return textResp(req.ID, fmt.Sprintf("Symbol %q not found", name))
		}
		def := defs[0]
		absPath, err := s.idx.Policy.ResolveWorkspacePath(def.File)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		data, err := os.ReadFile(absPath)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		if def.Language != LanguageGo {
			snippet, err := s.sourceRange(def.File, def.Line, def.Line+80)
			if err != nil {
				return errResp(req.ID, err.Error())
			}
			return textResp(req.ID, snippet)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "", data, 0)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		var fnDecl *ast.FuncDecl
		ast.Inspect(file, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok && fn.Name.Name == name {
				fnDecl = fn
				return false
			}
			return true
		})
		if fnDecl == nil {
			snippet, err := s.sourceRange(def.File, def.Line, def.Line+80)
			if err != nil {
				return errResp(req.ID, err.Error())
			}
			return textResp(req.ID, snippet)
		}
		start := fset.Position(fnDecl.Pos()).Line
		end := fset.Position(fnDecl.End()).Line
		snippet, err := s.sourceRange(def.File, start, end)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		return textResp(req.ID, snippet)

	case "scalpel_replace_symbol":
		file, _ := params.Arguments["file"].(string)
		name, _ := params.Arguments["name"].(string)
		replacement, _ := params.Arguments["replacement"].(string)
		if file == "" || name == "" || replacement == "" {
			return errResp(req.ID, "file, name and replacement required")
		}
		if s.ops == nil {
			return errResp(req.ID, "surgical ops unavailable")
		}
		if _, err := s.ops.ReplaceSymbol(file, name, replacement); err != nil {
			return errResp(req.ID, err.Error())
		}
		if err := s.idx.IndexFile(file, LanguageGo); err == nil {
			_ = s.idx.RefreshRouteGraph()
			_ = s.idx.buildEffectGraph()
		}
		return textResp(req.ID, fmt.Sprintf("Replaced symbol `%s` in %s", name, file))

	case "scalpel_add_struct_field":
		file, _ := params.Arguments["file"].(string)
		structName, _ := params.Arguments["struct"].(string)
		fieldSrc, _ := params.Arguments["field"].(string)
		if file == "" || structName == "" || fieldSrc == "" {
			return errResp(req.ID, "file, struct and field required")
		}
		if s.ops == nil {
			return errResp(req.ID, "surgical ops unavailable")
		}
		if _, err := s.ops.AddStructField(file, structName, fieldSrc); err != nil {
			return errResp(req.ID, err.Error())
		}
		if err := s.idx.IndexFile(file, LanguageGo); err == nil {
			_ = s.idx.RefreshRouteGraph()
			_ = s.idx.buildEffectGraph()
		}
		return textResp(req.ID, fmt.Sprintf("Added struct field to %s.%s", file, structName))

	case "scalpel_remove_struct_field":
		file, _ := params.Arguments["file"].(string)
		structName, _ := params.Arguments["struct"].(string)
		fieldName, _ := params.Arguments["field"].(string)
		if file == "" || structName == "" || fieldName == "" {
			return errResp(req.ID, "file, struct and field required")
		}
		if s.ops == nil {
			return errResp(req.ID, "surgical ops unavailable")
		}
		if _, err := s.ops.RemoveStructField(file, structName, fieldName); err != nil {
			return errResp(req.ID, err.Error())
		}
		if err := s.idx.IndexFile(file, LanguageGo); err == nil {
			_ = s.idx.RefreshRouteGraph()
			_ = s.idx.buildEffectGraph()
		}
		return textResp(req.ID, fmt.Sprintf("Removed struct field %s from %s.%s", fieldName, file, structName))

	case "scalpel_validate_code":
		code, _ := params.Arguments["code"].(string)
		language, _ := params.Arguments["language"].(string)
		if code == "" || language == "" {
			return errResp(req.ID, "code and language required")
		}
		constraints, err := LoadConstraints(s.idx.Policy.WorkspaceRoot)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		res := ValidateCodeInstrumented(context.Background(), code, language, constraints)
		if !res.Valid {
			var msgs []string
			for _, v := range res.Violations {
				msgs = append(msgs, fmt.Sprintf("%s: %s", v.ConstraintID, v.Message))
			}
			return textResp(req.ID, fmt.Sprintf("Violations:\n%s", strings.Join(msgs, "\n")))
		}
		return textResp(req.ID, "No violations")

	case "scalpel_module_contracts":
		module, _ := params.Arguments["module"].(string)
		if module == "" {
			return errResp(req.ID, "module required")
		}
		contractPath := workspaceSourcePath(s.idx.Policy.WorkspaceRoot, "module_contracts", module+".json")
		data, err := os.ReadFile(contractPath)
		if err != nil {
			return errResp(req.ID, fmt.Sprintf("contract for module %q not found", module))
		}
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, data, "", "  "); err != nil {
			return textResp(req.ID, string(data))
		}
		return textResp(req.ID, fmt.Sprintf("## Contract for module %s\n```json\n%s\n```", module, pretty.String()))

	case "scalpel_find_violations":
		module, _ := params.Arguments["module"].(string)
		if module == "" {
			return errResp(req.ID, "module required")
		}
		contractPath := workspaceSourcePath(s.idx.Policy.WorkspaceRoot, "module_contracts", module+".json")
		contractData, err := os.ReadFile(contractPath)
		if err != nil {
			return errResp(req.ID, "contract not found")
		}
		var contract struct {
			Exports []struct {
				Symbol  string         `json:"symbol"`
				Effects EffectModality `json:"effects"`
			} `json:"exports"`
		}
		if err := json.Unmarshal(contractData, &contract); err != nil {
			return errResp(req.ID, err.Error())
		}
		graph, err := s.idx.GetEffectGraph()
		if err != nil || graph == nil {
			return errResp(req.ID, "effect graph not available")
		}
		var violations []string
		for _, exp := range contract.Exports {
			actual, ok := graph.Symbols[exp.Symbol]
			if !ok {
				violations = append(violations, fmt.Sprintf("symbol %q not found in effect graph", exp.Symbol))
				continue
			}
			if exp.Effects.Error != actual.Error {
				violations = append(violations, fmt.Sprintf("%s: expected error:%s, actual:%s", exp.Symbol, exp.Effects.Error, actual.Error))
			}
			if exp.Effects.IO != actual.IO {
				violations = append(violations, fmt.Sprintf("%s: expected io:%s, actual:%s", exp.Symbol, exp.Effects.IO, actual.IO))
			}
			if exp.Effects.Panic != actual.Panic {
				violations = append(violations, fmt.Sprintf("%s: expected panic:%s, actual:%s", exp.Symbol, exp.Effects.Panic, actual.Panic))
			}
			if exp.Effects.Async != actual.Async {
				violations = append(violations, fmt.Sprintf("%s: expected async:%s, actual:%s", exp.Symbol, exp.Effects.Async, actual.Async))
			}
			if exp.Effects.Security != actual.Security {
				violations = append(violations, fmt.Sprintf("%s: expected security:%s, actual:%s", exp.Symbol, exp.Effects.Security, actual.Security))
			}
			if exp.Effects.Resource != actual.Resource {
				violations = append(violations, fmt.Sprintf("%s: expected resource:%s, actual:%s", exp.Symbol, exp.Effects.Resource, actual.Resource))
			}
		}
		if len(violations) == 0 {
			return textResp(req.ID, fmt.Sprintf("No violations for module %s", module))
		}
		return textResp(req.ID, "## Contract Violations\n"+strings.Join(violations, "\n"))

	case "scalpel_context_packet":
		task, _ := params.Arguments["task"].(string)
		module, _ := params.Arguments["module"].(string)
		if task == "" || module == "" {
			return errResp(req.ID, "task and module required")
		}
		var packet bytes.Buffer
		packet.WriteString(fmt.Sprintf("# Context Packet for: %s\nModule: %s\n\n", task, module))

		contractPath := workspaceSourcePath(s.idx.Policy.WorkspaceRoot, "module_contracts", module+".json")
		if contractData, err := os.ReadFile(contractPath); err == nil {
			packet.WriteString("## Module Contract\n```json\n")
			json.Indent(&packet, contractData, "", "  ")
			packet.WriteString("\n```\n\n")
		}

		graph, _ := s.idx.GetEffectGraph()
		if graph != nil {
			packet.WriteString("## Effect Modalities (relevant symbols)\n")
			for sym, mod := range graph.Symbols {
				belongs := false
				if s.store != nil && s.idx != nil {
					if defs := s.idx.FindDefinitions(sym); len(defs) > 0 {
						for _, def := range defs {
							if owner, ok, err := s.store.ModuleOwnerForPath(def.File); err == nil && ok && strings.EqualFold(owner.Name, module) {
								belongs = true
								break
							}
						}
					}
				}
				// Fallback to name heuristic
				if !belongs && strings.Contains(strings.ToLower(sym), strings.ToLower(module)) {
					belongs = true
				}
				if belongs {
					fmt.Fprintf(&packet, "- %s: err:%s io:%s panic:%s async:%s security:%s resource:%s\n", sym, mod.Error, mod.IO, mod.Panic, mod.Async, mod.Security, mod.Resource)
				}
			}
			packet.WriteString("\n")
		}

				sqlGraph, _ := LoadSQLGraph(workspaceStatePath(s.idx.Policy.WorkspaceRoot, "sql_graph.json"))
		if sqlGraph != nil {
			packet.WriteString("## SQL Tables\n")
			// Deduplicate tables by their column signatures to avoid multi-tenant bloat
			seenSignatures := make(map[string]string) // sig -> first table name seen
			printedCount := 0
			for name, table := range sqlGraph.Tables {
				if strings.EqualFold(table.Module, module) {
					// Create a simple signature based on columns
					var cols []string
					for _, c := range table.Columns {
						cols = append(cols, c.Name+":"+c.Type)
					}
					sig := strings.Join(cols, ",")

					if existingTable, exists := seenSignatures[sig]; exists {
						_ = existingTable
					} else {
						seenSignatures[sig] = name
						fmt.Fprintf(&packet, "- %s (%s)\n", name, table.File)
						printedCount++
					}
				}
			}

			// To output the missing message: Let's count how many tables belong to the module
			totalModuleTables := 0
			for _, table := range sqlGraph.Tables {
				if strings.EqualFold(table.Module, module) {
					totalModuleTables++
				}
			}

			if totalModuleTables > 0 && printedCount < totalModuleTables {
				fmt.Fprintf(&packet, "(%d identical tenant table variants omitted to save context space)\n", totalModuleTables-printedCount)
			}
			packet.WriteString("\n")
		}
		protoGraph, _ := LoadProtoGraph(workspaceStatePath(s.idx.Policy.WorkspaceRoot, "proto_graph.json"))
		if protoGraph != nil {
			packet.WriteString("## gRPC Services\n")
			for svcName, svc := range protoGraph.Services {
				belongs := false
				if strings.Contains(strings.ToLower(svcName), strings.ToLower(module)) {
					belongs = true
				} else if svc.File != "" && s.store != nil {
					// Primary: check if the .proto file is owned by the module
					if owner, ok, err := s.store.ModuleOwnerForPath(svc.File); err == nil && ok && strings.EqualFold(owner.Name, module) {
						belongs = true
					}
				}
				if !belongs && s.store != nil && s.idx != nil {
					// Secondary: check if any method is referenced by code owned by the module
					for _, method := range svc.Methods {
						refs := s.idx.FindReferences(method.Name)
						for _, ref := range refs {
							if owner, ok, err := s.store.ModuleOwnerForPath(ref.File); err == nil && ok && strings.EqualFold(owner.Name, module) {
								belongs = true
								break
							}
						}
						if belongs {
							break
						}
					}
				}
				if belongs {
					fmt.Fprintf(&packet, "- %s\n", svcName)
				}
			}
			packet.WriteString("\n")
		}

		constraints, _ := LoadConstraints(s.idx.Policy.WorkspaceRoot)
		if len(constraints) > 0 {
			packet.WriteString("## Architectural Constraints\n")
			for _, c := range constraints {
				fmt.Fprintf(&packet, "- [%s] %s: %s\n", c.ID, c.Name, c.Message)
			}
			packet.WriteString("\n")
		}

		decisionsPath := workspaceSourcePath(s.idx.Policy.WorkspaceRoot, "decisions.md")
		if decData, err := os.ReadFile(decisionsPath); err == nil {
			lines := strings.Split(string(decData), "\n")
			last := 20
			if len(lines) < last {
				last = len(lines)
			}
			packet.WriteString("## Recent Decisions (last 20 lines)\n```\n")
			for _, l := range lines[len(lines)-last:] {
				packet.WriteString(l + "\n")
			}
			packet.WriteString("```\n")
		}
		return textResp(req.ID, packet.String())

	case "scalpel_schema_surface":
		module, _ := params.Arguments["module"].(string)
		if module == "" {
			return errResp(req.ID, "module required")
		}
		graph, err := LoadSQLGraph(workspaceStatePath(s.idx.Policy.WorkspaceRoot, "sql_graph.json"))
		if err != nil || graph == nil {
			return errResp(req.ID, "sql_graph not built yet")
		}
		var tables []TableDef
		for _, t := range graph.Tables {
			if t.Module == module {
				tables = append(tables, t)
			}
		}
		out, _ := json.MarshalIndent(tables, "", "  ")
		return textResp(req.ID, fmt.Sprintf("## Schema for module %s\n```json\n%s\n```", module, out))

	case "scalpel_proto_surface":
		service, _ := params.Arguments["service"].(string)
		if service == "" {
			return errResp(req.ID, "service required")
		}
		graph, err := LoadProtoGraph(workspaceStatePath(s.idx.Policy.WorkspaceRoot, "proto_graph.json"))
		if err != nil || graph == nil {
			return errResp(req.ID, "proto_graph not built yet")
		}
		svc, ok := graph.Services[service]
		if !ok {
			return textResp(req.ID, fmt.Sprintf("Service %q not found", service))
		}
		out, _ := json.MarshalIndent(svc, "", "  ")
		return textResp(req.ID, fmt.Sprintf("## Service %s\n```json\n%s\n```", service, out))

	case "scalpel_vue_surface":
		module, _ := params.Arguments["module"].(string)
		if module == "" {
			return errResp(req.ID, "module required")
		}
		graph, err := LoadVueSurface(workspaceStatePath(s.idx.Policy.WorkspaceRoot, "vue_surface.json"))
		if err != nil || graph == nil {
			return errResp(req.ID, "vue_surface not built yet")
		}
		var comps []VueComponent
		for _, c := range graph.Components {
			if c.Module == module {
				comps = append(comps, c)
			}
		}
		out, _ := json.MarshalIndent(comps, "", "  ")
		return textResp(req.ID, fmt.Sprintf("## Vue components for module %s\n```json\n%s\n```", module, out))

	case "scalpel_sfc_read_block":
		file, _ := params.Arguments["file"].(string)
		blockType, _ := params.Arguments["block"].(string)
		if file == "" || blockType == "" {
			return errResp(req.ID, "file and block required")
		}
		absPath, err := s.idx.Policy.ResolveWorkspacePath(file)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		data, err := os.ReadFile(absPath)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		blocks, err := ParseVueSFC(bytes.NewReader(data))
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		for _, block := range blocks {
			if block.Tag == blockType {
				out := fmt.Sprintf("## %s block from %s\n```%s\n%s\n```", blockType, file, blockType, block.Content)
				if blockType == "script" {
					bindingSurface := extractBindingSurface(data)
					out += "\n## Binding Surface (template identifiers)\n" + bindingSurface
				}
				return textResp(req.ID, out)
			}
		}
		return errResp(req.ID, fmt.Sprintf("block %q not found in %s", blockType, file))

	case "scalpel_sfc_replace_block":
		file, _ := params.Arguments["file"].(string)
		blockType, _ := params.Arguments["block"].(string)
		newContent, _ := params.Arguments["content"].(string)
		if file == "" || blockType == "" {
			return errResp(req.ID, "file and block required")
		}
		absPath, err := s.idx.Policy.ResolveWorkspacePath(file)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		src, err := os.ReadFile(absPath)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		blocks, err := ParseVueSFC(bytes.NewReader(src))
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		modified := false
		for i := range blocks {
			if blocks[i].Tag == blockType {
				blocks[i].Content = newContent
				modified = true
				break
			}
		}
		if !modified {
			return errResp(req.ID, fmt.Sprintf("block %q not found in %s", blockType, file))
		}
		newSource := ReplaceSFCBlocks(src, blocks)
		cst, err := s.idx.Pool.ParseSource(file, LanguageVue, newSource)
		if err != nil {
			return errResp(req.ID, fmt.Sprintf("syntax validation failed for %s: %v", file, err))
		}
		defer cst.Release()
		if cst.HasError() {
			return errResp(req.ID, fmt.Sprintf("syntax validation failed for %s: parse errors detected", file))
		}
		if err := os.WriteFile(absPath, newSource, 0644); err != nil {
			return errResp(req.ID, err.Error())
		}
		lang, _ := DetectOraLanguage(file)
		if err := s.idx.IndexFile(file, lang); err == nil {
			_ = s.idx.RefreshRouteGraph()
		}
		return textResp(req.ID, fmt.Sprintf("Replaced %s block in %s", blockType, file))

	case "scalpel_git_changes":
		since, _ := params.Arguments["since"].(string)
		if since == "" {
			since = "HEAD~1"
		}
		cmd := exec.Command("git", "diff", "--name-only", since)
		cmd.Dir = s.idx.Policy.WorkspaceRoot
		output, err := cmd.Output()
		if err != nil {
			return errResp(req.ID, fmt.Sprintf("git diff failed: %v", err))
		}
		files := strings.Split(strings.TrimSpace(string(output)), "\n")
		var filtered []string
		for _, f := range files {
			if f != "" && (strings.HasSuffix(f, ".go") || strings.HasSuffix(f, ".vue") || strings.HasSuffix(f, ".ts")) {
				filtered = append(filtered, f)
			}
		}
		if len(filtered) == 0 {
			return textResp(req.ID, "No changed files found.")
		}
		out := fmt.Sprintf("## Git changed files since %s\n", since)
		for _, f := range filtered {
			out += fmt.Sprintf("- %s\n", f)
		}
		return textResp(req.ID, out)

	case "scalpel_schema_breadcrumbs":
		table, _ := params.Arguments["table"].(string)
		if table == "" {
			return errResp(req.ID, "table required")
		}
		graphPath := workspaceStatePath(s.idx.Policy.WorkspaceRoot, "sql_graph.json")
		sqlGraph, err := LoadSQLGraph(graphPath)
		if err != nil || sqlGraph == nil {
			return errResp(req.ID, "sql_graph not available")
		}
		tableDef, ok := sqlGraph.Tables[table]
		if !ok {
			return textResp(req.ID, fmt.Sprintf("Table %q not found in graph", table))
		}
		var parents []string
		for _, t := range sqlGraph.Tables {
			for _, fk := range t.ForeignKeys {
				if fk.RefTable == table {
					parents = append(parents, t.Name)
				}
			}
		}
		var children []string
		for _, fk := range tableDef.ForeignKeys {
			children = append(children, fk.RefTable)
		}
		out := fmt.Sprintf("## Table relationships for `%s`\n", table)
		out += fmt.Sprintf("- Tenant scoped: %v\n", tableDef.IsTenant)
		out += fmt.Sprintf("- Base name: %s\n", tableDef.BaseName)
		if len(parents) > 0 {
			out += "- Referenced by: " + strings.Join(parents, ", ") + "\n"
		}
		if len(children) > 0 {
			out += "- References: " + strings.Join(children, ", ") + "\n"
		}
		return textResp(req.ID, out)

	case "scalpel_frontend_api_calls":
		component, _ := params.Arguments["component"].(string)
		apiPath := workspaceStatePath(s.idx.Policy.WorkspaceRoot, "frontend_api.json")
		fIdx, err := LoadFrontendAPIIndex(apiPath)
		if err != nil || fIdx == nil {
			return errResp(req.ID, "frontend API index not available")
		}
		var calls []APICall
		for _, c := range fIdx.Calls {
			if component == "" || strings.Contains(c.Component, component) {
				calls = append(calls, c)
			}
		}
		if len(calls) == 0 {
			return textResp(req.ID, "No API calls found.")
		}
		out := "## Frontend API Calls\n"
		for _, c := range calls {
			out += fmt.Sprintf("- %s:%d | %s %s | %s\n", c.Component, c.Line, c.Method, c.URL, c.Function)
		}
		return textResp(req.ID, out)

	case "scalpel_plan_edit":
		task, _ := params.Arguments["task"].(string)
		symbol, _ := params.Arguments["symbol"].(string)
		fileHint, _ := params.Arguments["file"].(string)
		if symbol == "" && task == "" {
			return errResp(req.ID, "task or symbol required")
		}
		plan, err := s.PlanEdit(task, symbol, fileHint)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		out, _ := json.MarshalIndent(plan, "", "  ")
		return textResp(req.ID, fmt.Sprintf("## Edit Plan\n```json\n%s\n```", out))

	case "scalpel_apply_patch_preview":
		apply, _ := params.Arguments["apply"].(bool)
		if patchesArg, ok := params.Arguments["patches"].([]interface{}); ok && len(patchesArg) > 0 {
			var hunks []PatchHunk
			for _, raw := range patchesArg {
				patchMap, ok := raw.(map[string]interface{})
				if !ok {
					return errResp(req.ID, "invalid patch entry")
				}
				hunks = append(hunks, PatchHunk{
					File:       stringArg(patchMap, "file"),
					StartLine:  intArg(patchMap, "start_line", 0),
					EndLine:    intArg(patchMap, "end_line", 0),
					NewContent: stringArg(patchMap, "new_content"),
				})
			}
			previews, err := s.ApplyPatchPreviewBatch(hunks, apply)
			if err != nil {
				return errResp(req.ID, err.Error())
			}
			out, _ := json.MarshalIndent(previews, "", "  ")
			return textResp(req.ID, fmt.Sprintf("## Patch Preview\n```json\n%s\n```", out))
		}

		file, _ := params.Arguments["file"].(string)
		start := intArg(params.Arguments, "start_line", 0)
		end := intArg(params.Arguments, "end_line", 0)
		newContent, _ := params.Arguments["new_content"].(string)
		if file == "" || newContent == "" {
			return errResp(req.ID, "file and new_content required")
		}
		if start == 0 || end == 0 {
			if anchor, ok := params.Arguments["symbol_anchor"].(string); ok && anchor != "" {
				f, sLine, eLine, err := s.resolveSymbolLocation(anchor)
				if err == nil {
					file = f
					start = sLine
					end = eLine
				}
			}
		}
		if start == 0 || end == 0 {
			return errResp(req.ID, "start_line and end_line required (or symbol_anchor)")
		}
		preview, err := s.ApplyPatchPreview(file, start, end, newContent, apply)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		out, _ := json.MarshalIndent(preview, "", "  ")
		return textResp(req.ID, fmt.Sprintf("## Patch Preview\n```json\n%s\n```", out))

	case "scalpel_replace_symbol_vue":
		file, _ := params.Arguments["file"].(string)
		symbol, _ := params.Arguments["symbol"].(string)
		replacement, _ := params.Arguments["replacement"].(string)
		if file == "" || symbol == "" || replacement == "" {
			return errResp(req.ID, "file, symbol and replacement required")
		}
		fops := NewFrontendOps(s.idx)
		if err := fops.ReplaceSymbolInVue(file, symbol, replacement); err != nil {
			return errResp(req.ID, err.Error())
		}
		return textResp(req.ID, fmt.Sprintf("Replaced symbol `%s` in %s", symbol, file))

	case "scalpel_add_import_vue":
		file, _ := params.Arguments["file"].(string)
		importPath, _ := params.Arguments["import_path"].(string)
		if file == "" || importPath == "" {
			return errResp(req.ID, "file and import_path required")
		}
		fops := NewFrontendOps(s.idx)
		if err := fops.AddImportToVue(file, importPath); err != nil {
			return errResp(req.ID, err.Error())
		}
		return textResp(req.ID, fmt.Sprintf("Added import to %s", file))

	case "scalpel_add_composable_vue":
		file, _ := params.Arguments["file"].(string)
		call, _ := params.Arguments["call"].(string)
		if file == "" || call == "" {
			return errResp(req.ID, "file and call required")
		}
		fops := NewFrontendOps(s.idx)
		if err := fops.AddComposableToSetup(file, call); err != nil {
			return errResp(req.ID, err.Error())
		}
		return textResp(req.ID, fmt.Sprintf("Added composable call `%s` to %s", call, file))

	// ===== PHASE 3: SURGICAL EXECUTION =====
	case "scalpel_patch_symbol_body":
		file, _ := params.Arguments["file"].(string)
		symbol, _ := params.Arguments["symbol"].(string)
		search, _ := params.Arguments["search"].(string)
		replace, _ := params.Arguments["replace"].(string)
		if file == "" || symbol == "" || search == "" || replace == "" {
			return errResp(req.ID, "file, symbol, search, replace required")
		}
		if s.ops == nil {
			return errResp(req.ID, "surgical ops unavailable")
		}
		result, err := s.ops.PatchSymbolBody(file, symbol, search, replace)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		if result.Changed {
			_ = s.idx.IndexFile(file, LanguageGo)
			_ = s.idx.RefreshRouteGraph()
		}
		if confVal, ok := params.Arguments["confidence"].(float64); ok {
			s.confStore.Record(fmt.Sprintf("patch:%s:%s", file, symbol), int(confVal))
		}
		return textResp(req.ID, fmt.Sprintf("Patched body of `%s` in %s", symbol, file))

	case "scalpel_batch_edit":
		patchesRaw, _ := params.Arguments["patches"].([]interface{})
		if len(patchesRaw) == 0 {
			return errResp(req.ID, "patches array is required")
		}
		patches := make([]BatchPatch, 0, len(patchesRaw))
		for _, raw := range patchesRaw {
			if rawMap, ok := raw.(map[string]interface{}); ok {
				data, _ := json.Marshal(rawMap)
				var p BatchPatch
				if err := json.Unmarshal(data, &p); err != nil {
					return errResp(req.ID, "invalid patch: "+err.Error())
				}
				patches = append(patches, p)
			}
		}
		opts := BatchEditOptions{
			DryRun:                  getBool(params.Arguments, "dry_run"),
			Atomic:                  getBool(params.Arguments, "atomic"),
			StopOnFirstGroupFailure: getBool(params.Arguments, "stop_on_first_group_failure"),
			ReadBefore:              getBool(params.Arguments, "read_before"),
			TraceBefore:             getBool(params.Arguments, "trace_before"),
			CheckEffects:            getBool(params.Arguments, "check_effects"),
			ValidateAfter:           getBool(params.Arguments, "validate_after"),
			RecordEdit:              getBool(params.Arguments, "record_edit"),
			CleanupImports:          getBool(params.Arguments, "cleanup_imports"),
			ValidateWiring:          getBool(params.Arguments, "validate_wiring"),
			Timeout:                 5 * time.Second,
		}
		result, err := s.BatchEdit(patches, opts)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		out, _ := json.MarshalIndent(result, "", "  ")
		return textResp(req.ID, fmt.Sprintf("## Batch Edit Result\n```json\n%s\n```", out))

	case "scalpel_scaffold":
		manifestPath, _ := params.Arguments["manifest"].(string)
		if manifestPath == "" {
			return errResp(req.ID, "manifest path is required")
		}
		dryRun, _ := params.Arguments["dry_run"].(bool)
		created, err := s.Scaffold(manifestPath, dryRun)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		return textResp(req.ID, fmt.Sprintf("Created files:\n%s", strings.Join(created, "\n")))

	case "scalpel_inspect_symbol":
		name, _ := params.Arguments["name"].(string)
		if name == "" {
			return errResp(req.ID, "name is required")
		}
		mode, _ := params.Arguments["mode"].(string)
		if mode == "" {
			mode = "context"
		}
		switch mode {
		case "definition":
			syms := s.idx.FindDefinitions(name)
			if len(syms) == 0 {
				return textResp(req.ID, fmt.Sprintf("No symbol found: %q", name))
			}
			out := fmt.Sprintf("## Definitions for `%s`\n", name)
			for _, sym := range syms {
				out += fmt.Sprintf("- `%s` kind=%s lang=%s → %s:%d\n", sym.Name, sym.Kind, sym.Language, sym.File, sym.Line)
			}
			return textResp(req.ID, out)
		case "refs":
			refs := s.findRefs(name)
			if len(refs) == 0 {
				return textResp(req.ID, fmt.Sprintf("No references found for `%s`.", name))
			}
			out := fmt.Sprintf("## References to `%s` (%d sites)\n", name, len(refs))
			for _, r := range refs {
				out += fmt.Sprintf("- %s:%d (lang=%s)\n", r.File, r.Line, r.Language)
			}
			return textResp(req.ID, out)
		case "body":
			text, err := s.readSymbol(name)
			if err != nil {
				return errResp(req.ID, err.Error())
			}
			return textResp(req.ID, text)
		default:
			contextLines := intArg(params.Arguments, "contextLines", 8)
			if contextLines < 0 {
				contextLines = 0
			}
			if contextLines > 40 {
				contextLines = 40
			}
			includeRefs, _ := params.Arguments["includeRefs"].(bool)
			text, err := s.symbolContext(name, contextLines, includeRefs)
			if err != nil {
				return errResp(req.ID, err.Error())
			}
			return textResp(req.ID, text)
		}

	case "scalpel_edit_log":
		if s.store == nil {
			return errResp(req.ID, "edit store not available")
		}
		filter, _ := params.Arguments["filter"].(string)
		limit := intArg(params.Arguments, "limit", 10)
		if limit < 1 {
			limit = 10
		}
		if limit > 100 {
			limit = 100
		}
		if strings.HasPrefix(filter, "file:") {
			edits, err := s.store.EditsForFile(strings.TrimPrefix(filter, "file:"), limit)
			if err != nil {
				return errResp(req.ID, err.Error())
			}
			return textResp(req.ID, renderEditEntries("Edits for file", edits))
		}
		if strings.HasPrefix(filter, "symbol:") {
			edits, err := s.store.EditsForSymbol(strings.TrimPrefix(filter, "symbol:"), limit)
			if err != nil {
				return errResp(req.ID, err.Error())
			}
			return textResp(req.ID, renderEditEntries("Edits for symbol", edits))
		}
		edits, err := s.store.RecentEdits(limit)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		return textResp(req.ID, renderEditEntries("Recent Edits", edits))

	case "scalpel_inspect_route":
		mode, _ := params.Arguments["mode"].(string)
		if mode == "" {
			mode = "list"
		}
		switch mode {
		case "trace":
			url, _ := params.Arguments["url"].(string)
			if url == "" {
				return errResp(req.ID, "url is required for mode=trace")
			}
			method, _ := params.Arguments["method"].(string)
			depth := intArg(params.Arguments, "depth", 1)
			if depth < 1 {
				depth = 1
			}
			if depth > 5 {
				depth = 5
			}
			text, err := s.traceRequest(url, method, 20, depth)
			if err != nil {
				return errResp(req.ID, err.Error())
			}
			return textResp(req.ID, text)
		case "chain":
			url, _ := params.Arguments["url"].(string)
			if url == "" {
				return errResp(req.ID, "url is required for mode=chain")
			}
			method, _ := params.Arguments["method"].(string)
			depth := intArg(params.Arguments, "depth", 4)
			if depth < 1 {
				depth = 1
			}
			if depth > 8 {
				depth = 8
			}
			text, err := s.traceChain(url, method, depth)
			if err != nil {
				return errResp(req.ID, err.Error())
			}
			return textResp(req.ID, text)
		default:
			pathFilter, _ := params.Arguments["path"].(string)
			limit := intArg(params.Arguments, "limit", 200)
			if limit < 1 {
				limit = 200
			}
			text, err := s.listRoutes(pathFilter, limit)
			if err != nil {
				return errResp(req.ID, err.Error())
			}
			return textResp(req.ID, text)
		}

	case "scalpel_effect":
		mode, _ := params.Arguments["mode"].(string)
		if mode == "" {
			mode = "modality"
		}
		switch mode {
		case "version":
			graph, err := s.idx.GetEffectGraph()
			if err != nil {
				return errResp(req.ID, err.Error())
			}
			if graph == nil {
				return textResp(req.ID, "No effect graph built yet.")
			}
			return textResp(req.ID, fmt.Sprintf("effect_graph version %d (generated %s)", graph.Version, graph.Generated.Format(time.RFC3339)))
		case "trace":
			name, _ := params.Arguments["name"].(string)
			if name == "" {
				return errResp(req.ID, "name required")
			}
			depth := intArg(params.Arguments, "depth", 3)
			text, err := s.traceEffects(name, depth)
			if err != nil {
				return errResp(req.ID, err.Error())
			}
			return textResp(req.ID, text)
		default:
			name, _ := params.Arguments["name"].(string)
			if name == "" {
				return errResp(req.ID, "name required")
			}
			graph, err := s.idx.GetEffectGraph()
			if err != nil {
				return errResp(req.ID, fmt.Sprintf("effect graph error: %v", err))
			}
			if graph == nil {
				return textResp(req.ID, "Effect graph not built yet. Run reindex.")
			}
			mod, ok := graph.Symbols[name]
			if !ok {
				return textResp(req.ID, fmt.Sprintf("No effect data for symbol %q", name))
			}
			out := fmt.Sprintf("## Effect Modalities for `%s`\n", name)
			out += fmt.Sprintf("- error: %s\n- io: %s\n- panic: %s\n- async: %s\n- security: %s\n- resource: %s\n- confidence: %d%%\n",
				mod.Error, mod.IO, mod.Panic, mod.Async, mod.Security, mod.Resource, mod.Confidence)
			out += fmt.Sprintf("\nGraph version: %d, generated: %s\n", graph.Version, graph.Generated.Format(time.RFC3339))
			return textResp(req.ID, out)
		}

	case "scalpel_surface":
		kind, _ := params.Arguments["kind"].(string)
		name, _ := params.Arguments["name"].(string)
		if name == "" {
			return errResp(req.ID, "name is required")
		}
		switch kind {
		case "proto":
			graph, err := LoadProtoGraph(workspaceStatePath(s.idx.Policy.WorkspaceRoot, "proto_graph.json"))
			if err != nil || graph == nil {
				return errResp(req.ID, "proto_graph not built yet")
			}
			svc, ok := graph.Services[name]
			if !ok {
				return textResp(req.ID, fmt.Sprintf("Service %q not found", name))
			}
			out, _ := json.MarshalIndent(svc, "", "  ")
			return textResp(req.ID, fmt.Sprintf("## Service %s\n```json\n%s\n```", name, out))
		case "vue":
			graph, err := LoadVueSurface(workspaceStatePath(s.idx.Policy.WorkspaceRoot, "vue_surface.json"))
			if err != nil || graph == nil {
				return errResp(req.ID, "vue_surface not built yet")
			}
			var comps []VueComponent
			for _, c := range graph.Components {
				if c.Module == name {
					comps = append(comps, c)
				}
			}
			out, _ := json.MarshalIndent(comps, "", "  ")
			return textResp(req.ID, fmt.Sprintf("## Vue components for module %s\n```json\n%s\n```", name, out))
		default:
			graph, err := LoadSQLGraph(workspaceStatePath(s.idx.Policy.WorkspaceRoot, "sql_graph.json"))
			if err != nil || graph == nil {
				return errResp(req.ID, "sql_graph not built yet")
			}
			var tables []TableDef
			for _, t := range graph.Tables {
				if t.Module == name {
					tables = append(tables, t)
				}
			}
			out, _ := json.MarshalIndent(tables, "", "  ")
			return textResp(req.ID, fmt.Sprintf("## Schema for module %s\n```json\n%s\n```", name, out))
		}

	case "scalpel_validate":
		mode, _ := params.Arguments["mode"].(string)
		if mode == "" {
			mode = "code"
		}
		switch mode {
		case "contract":
			module, _ := params.Arguments["module"].(string)
			if module == "" {
				return errResp(req.ID, "module required for mode=contract")
			}
			contractPath := workspaceSourcePath(s.idx.Policy.WorkspaceRoot, "module_contracts", module+".json")
			contractData, err := os.ReadFile(contractPath)
			if err != nil {
				return errResp(req.ID, "contract not found")
			}
			var contract struct {
				Exports []struct {
					Symbol  string         `json:"symbol"`
					Effects EffectModality `json:"effects"`
				} `json:"exports"`
			}
			if err := json.Unmarshal(contractData, &contract); err != nil {
				return errResp(req.ID, err.Error())
			}
			graph, err := s.idx.GetEffectGraph()
			if err != nil || graph == nil {
				return errResp(req.ID, "effect graph not available")
			}
			var violations []string
			for _, exp := range contract.Exports {
				actual, ok := graph.Symbols[exp.Symbol]
				if !ok {
					violations = append(violations, fmt.Sprintf("symbol %q not found in effect graph", exp.Symbol))
					continue
				}
				if exp.Effects.Error != actual.Error {
					violations = append(violations, fmt.Sprintf("%s: expected error:%s, actual:%s", exp.Symbol, exp.Effects.Error, actual.Error))
				}
				if exp.Effects.IO != actual.IO {
					violations = append(violations, fmt.Sprintf("%s: expected io:%s, actual:%s", exp.Symbol, exp.Effects.IO, actual.IO))
				}
				if exp.Effects.Panic != actual.Panic {
					violations = append(violations, fmt.Sprintf("%s: expected panic:%s, actual:%s", exp.Symbol, exp.Effects.Panic, actual.Panic))
				}
			}
			if len(violations) == 0 {
				return textResp(req.ID, fmt.Sprintf("No violations for module %s", module))
			}
			return textResp(req.ID, "## Contract Violations\n"+strings.Join(violations, "\n"))
		case "policy":
			policyPath, _ := params.Arguments["policy"].(string)
			if policyPath == "" {
				return errResp(req.ID, "policy path required")
			}
			violations, err := s.idx.RunPolicy(policyPath)
			if err != nil {
				return errResp(req.ID, err.Error())
			}
			if len(violations) == 0 {
				return textResp(req.ID, "Policy passed – no violations found.")
			}
			return textResp(req.ID, "## Policy Violations\n"+strings.Join(violations, "\n"))
		default:
			code, _ := params.Arguments["code"].(string)
			language, _ := params.Arguments["language"].(string)
			if code == "" || language == "" {
				return errResp(req.ID, "code and language required")
			}
			constraints, err := LoadConstraints(s.idx.Policy.WorkspaceRoot)
			if err != nil {
				return errResp(req.ID, err.Error())
			}
			res := ValidateCodeInstrumented(context.Background(), code, language, constraints)
			if !res.Valid {
				var msgs []string
				for _, v := range res.Violations {
					msgs = append(msgs, fmt.Sprintf("%s: %s", v.ConstraintID, v.Message))
				}
				return textResp(req.ID, fmt.Sprintf("Violations:\n%s", strings.Join(msgs, "\n")))
			}
			return textResp(req.ID, "No violations")
		}

	// ===== PHASE 1: SEMANTIC SEARCH =====
	case "scalpel_semantic_search":
		query, _ := params.Arguments["query"].(string)
		limit := intArg(params.Arguments, "limit", 5)
		includeScores, _ := params.Arguments["include_scores"].(bool)
		if query == "" {
			return errResp(req.ID, "query is required")
		}

		if s.semanticEngine == nil {
			return errResp(req.ID, "Semantic Engine is not initialized.")
		}

		// On-demand hydration: if the vector store is empty, kick off a
		// background build so a future query picks up fresh vectors.
		// The current call will serve cached results (or fallback linear search)
		// via the existing IsStale mechanism in HybridSearch.
		s.semanticEngine.mu.RLock()
		docsCount := len(s.semanticEngine.Documents)
		s.semanticEngine.mu.RUnlock()

		if docsCount == 0 && s.semanticEngine != nil && !s.semanticEngine.WarmingUp.Load() {
			go func() {
				fmt.Fprintf(os.Stderr, "[oracode] semantic index build triggered on-demand by search query\n")
				s.BuildSemanticIndex()
			}()
		}

				store, _ := params.Arguments["store"].(string)
		if store == "" { store = "code" } // Default to code if unspecified

		results, err := s.semanticEngine.HybridSearch(query, limit, store)
		if err != nil {
			return errResp(req.ID, "Semantic search failed: "+err.Error())
		}

		if len(results) == 0 {
			return textResp(req.ID, "No semantically relevant code found. Embeddings may still be generating in the background.")
		}

		var out strings.Builder
		fmt.Fprintf(&out, "## Hybrid Search Results for: '%s'\n\n", query)
		for _, res := range results {
			fmt.Fprintf(&out, "### `%s` (in %s)\n", res.Symbol, res.File)

			if includeScores {
				fmt.Fprintf(&out, "> **Similarity Indices:** Fused RRF: `%.4f` | Semantic Vector (Cosine): `%.4f` | Keyword BM25: `%.4f`\n",
					res.FusedScore, res.VectorScore, res.BM25Score)
			}

			fmt.Fprintf(&out, "**Signature:** `%s`\n", res.Signature)
			fmt.Fprintf(&out, "**Known Effects:** `%s`\n", res.Effects)
			fmt.Fprintf(&out, "```go\n%s\n```\n\n", res.Content)
		}
		return textResp(req.ID, out.String())

	// ===== PHASE 4: DEBUGGING & VALIDATION =====
	case "scalpel_verify_with_context":
		eng := &Engine{WorkspaceRoot: s.idx.Policy.WorkspaceRoot, Security: s.idx.Policy}
		failure := eng.VerifyWithContext()
		out, _ := json.MarshalIndent(failure, "", "  ")
		if failure.Passed {
			return textResp(req.ID, "## Build Verification\n```json\n"+string(out)+"\n```")
		}
		return textResp(req.ID, "## Build Failed\n```json\n"+string(out)+"\n```")

	case "scalpel_evaluate_pattern":
		snippet, _ := params.Arguments["snippet"].(string)
		language, _ := params.Arguments["language"].(string)
		scope, _ := params.Arguments["scope"].(string)
		if snippet == "" || language == "" {
			return errResp(req.ID, "snippet and language are required")
		}
		report, err := s.EvaluatePattern(snippet, language, scope)
		if err != nil {
			return errResp(req.ID, err.Error())
		}
		return textResp(req.ID, report)

	// ===== VUE DIRECTIVE INJECTION =====
	case "scalpel_vue_inject_directive":
		file, _ := params.Arguments["file"].(string)
		tag, _ := params.Arguments["tag"].(string)
		matchAttr, _ := params.Arguments["match_attr"].(string)
		directive, _ := params.Arguments["directive"].(string)
		dryRun := getBool(params.Arguments, "dry_run")

		if file == "" || tag == "" || directive == "" {
			return errResp(req.ID, "file, tag, and directive are required")
		}

		fops := NewFrontendOps(s.idx)
		if fops == nil {
			return errResp(req.ID, "frontend operations not available")
		}

		res, err := fops.InjectVueDirective(file, tag, matchAttr, directive, dryRun)
		if err != nil {
			return errResp(req.ID, err.Error())
		}

		out, _ := json.MarshalIndent(res, "", "  ")
		return textResp(req.ID, fmt.Sprintf("## Vue Directive Injection Result\n```json\n%s\n```", out))

	// ===== GOVERNANCE =====
	case "scalpel_read_team_workflow":
		workflowName, _ := params.Arguments["workflow_name"].(string)
		workflowsDir := workspaceSourcePath(s.idx.Policy.WorkspaceRoot, "workflows")
		if workflowName == "" {
			files, err := os.ReadDir(workflowsDir)
			if err != nil {
				return textResp(req.ID, "No team workflows defined yet.")
			}
			var list []string
			for _, f := range files {
				if !f.IsDir() && strings.HasSuffix(f.Name(), ".md") {
					list = append(list, "- "+f.Name())
				}
			}
			return textResp(req.ID, "## Available Team Workflows\n"+strings.Join(list, "\n"))
		}
		if !strings.HasSuffix(workflowName, ".md") {
			workflowName += ".md"
		}
		data, err := os.ReadFile(filepath.Join(workflowsDir, workflowName))
		if err != nil {
			return errResp(req.ID, "workflow not found: "+workflowName)
		}
		return textResp(req.ID, fmt.Sprintf("## Workflow: %s\n\n%s", workflowName, string(data)))

	case "scalpel_log_decision":
		module, _ := params.Arguments["module"].(string)
		decision, _ := params.Arguments["decision"].(string)
		reasoning, _ := params.Arguments["reasoning"].(string)
		if module == "" || decision == "" {
			return errResp(req.ID, "module and decision required")
		}
		decisionsPath := workspaceSourcePath(s.idx.Policy.WorkspaceRoot, "decisions.md")
		entry := fmt.Sprintf("\n### %s - Module: %s\n**Decision:** %s\n**Reasoning:** %s\n",
			time.Now().Format("2006-01-02 15:04"), module, decision, reasoning)
		f, err := os.OpenFile(decisionsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return errResp(req.ID, "failed to log decision: "+err.Error())
		}
		f.WriteString(entry)
		f.Close()
		return textResp(req.ID, "Decision logged successfully.")

	default:
		return errResp(req.ID, "unknown tool: "+params.Name)
	}
}

// ========== Helper functions ==========
func stringArg(args map[string]interface{}, name string) string {
	v, ok := args[name]
	if !ok {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	case fmt.Stringer:
		return s.String()
	default:
		return ""
	}
}

func intArg(args map[string]interface{}, name string, fallback int) int {
	v, ok := args[name]
	if !ok {
		return fallback
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i
		}
	}
	return fallback
}

func textResp(id interface{}, text string) mcpResponse {
	return mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  mcpCallResult{Content: []mcpContent{{Type: "text", Text: text}}},
	}
}

func errResp(id interface{}, msg string) mcpResponse {
	return mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  mcpCallResult{Content: []mcpContent{{Type: "text", Text: "Error: " + msg}}, IsError: true},
	}
}

func sourceRangeCommon(absPath, file string, start, end int) (string, error) {
	if start < 1 {
		start = 1
	}
	if end < start {
		end = start
	}
	if end-start > 240 {
		end = start + 240
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", file, err)
	}
	lines := strings.Split(string(data), "\n")
	if start > len(lines) {
		return "", fmt.Errorf("start line %d beyond end of %s (%d lines)", start, file, len(lines))
	}
	if end > len(lines) {
		end = len(lines)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "## Source `%s`:%d-%d\n", file, start, end)
	for i := start; i <= end; i++ {
		fmt.Fprintf(&out, "%5d | %s\n", i, strings.TrimRight(lines[i-1], "\r"))
	}
	return out.String(), nil
}

func (s *MCPServer) sourceRange(file string, start, end int) (string, error) {
	absPath, err := s.idx.Policy.ResolveWorkspacePath(file)
	if err != nil {
		return "", err
	}
	return sourceRangeCommon(absPath, file, start, end)
}

func (s *MCPServer) findRefs(name string) []*Symbol {
	s.idx.mu.RLock()
	defer s.idx.mu.RUnlock()
	var refs []*Symbol
	for _, symList := range s.idx.Symbols {
		for _, sym := range symList {
			if sym.Name == name && sym.Kind == "reference" {
				refs = append(refs, sym)
			}
		}
	}
	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].File == refs[j].File {
			return refs[i].Line < refs[j].Line
		}
		return refs[i].File < refs[j].File
	})
	return refs
}

func (s *MCPServer) lookupFileSymbols(file string) []*Symbol {
	candidates := []string{
		file,
		filepath.Clean(file),
		filepath.ToSlash(filepath.Clean(file)),
		filepath.FromSlash(file),
	}
	s.idx.mu.RLock()
	defer s.idx.mu.RUnlock()
	for _, cand := range candidates {
		if syms := s.idx.FileSymbols[cand]; len(syms) > 0 {
			out := make([]*Symbol, len(syms))
			copy(out, syms)
			return out
		}
	}
	return nil
}

func (s *MCPServer) symbolContext(name string, contextLines int, includeRefs bool) (string, error) {
	defs := s.idx.FindDefinitions(name)
	if len(defs) == 0 {
		return "", fmt.Errorf("no definitions found for %q", name)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "## Symbol Context `%s`\n", name)
	for _, def := range defs {
		start := def.Line - contextLines
		if start < 1 {
			start = 1
		}
		end := def.Line + contextLines
		snippet, err := s.sourceRange(def.File, start, end)
		if err != nil {
			fmt.Fprintf(&out, "\n### %s:%d\nError: %v\n", def.File, def.Line, err)
			continue
		}
		fmt.Fprintf(&out, "\n### %s:%d kind=%s lang=%s\n%s", def.File, def.Line, def.Kind, def.Language, snippet)
	}
	if includeRefs {
		refs := s.findRefs(name)
		fmt.Fprintf(&out, "\n## References (%d)\n", len(refs))
		for _, ref := range refs {
			fmt.Fprintf(&out, "- %s:%d lang=%s\n", ref.File, ref.Line, ref.Language)
		}
	}
	return out.String(), nil
}

func (s *MCPServer) fileSymbols(file string) string {
	syms := s.lookupFileSymbols(file)
	if len(syms) == 0 {
		return fmt.Sprintf("No indexed symbols found in `%s`. Try `scalpel_reindex` for this file.", file)
	}
	sort.SliceStable(syms, func(i, j int) bool {
		if syms[i].Line == syms[j].Line {
			return syms[i].Name < syms[j].Name
		}
		return syms[i].Line < syms[j].Line
	})
	var out strings.Builder
	fmt.Fprintf(&out, "## Symbols in `%s`\n", file)
	for _, sym := range syms {
		if sym.Kind == "module" {
			fmt.Fprintf(&out, "- imports: %s\n", strings.Join(sym.Imports, ", "))
			continue
		}
		fmt.Fprintf(&out, "- `%s` kind=%s line=%d lang=%s\n", sym.Name, sym.Kind, sym.Line, sym.Language)
	}
	return out.String()
}

func (s *MCPServer) dependencies(file, symbol string) (string, error) {
	if file == "" && symbol != "" {
		defs := s.idx.FindDefinitions(symbol)
		if len(defs) == 0 {
			return "", fmt.Errorf("no definitions found for %q", symbol)
		}
		file = defs[0].File
	}
	if file == "" {
		return "", fmt.Errorf("file or symbol is required")
	}
	syms := s.lookupFileSymbols(file)
	if len(syms) == 0 {
		return fmt.Sprintf("No indexed symbols found in `%s`.", file), nil
	}
	depNames := make(map[string]bool)
	for _, sym := range syms {
		if sym.Kind == "reference" {
			depNames[sym.Name] = true
		}
	}
	names := make([]string, 0, len(depNames))
	for name := range depNames {
		names = append(names, name)
	}
	sort.Strings(names)
	var out strings.Builder
	fmt.Fprintf(&out, "## Dependencies for `%s`\n", file)
	if symbol != "" {
		fmt.Fprintf(&out, "Source symbol: `%s`\n\n", symbol)
	}
	if len(names) == 0 {
		out.WriteString("No call/reference dependencies indexed for this file.\n")
		return out.String(), nil
	}
	for _, n := range names {
		defs := s.idx.FindDefinitions(n)
		if len(defs) == 0 {
			fmt.Fprintf(&out, "- `%s` -> definition not indexed\n", n)
			continue
		}
		fmt.Fprintf(&out, "- `%s`\n", n)
		for _, def := range defs {
			if def.Kind == "definition" {
				fmt.Fprintf(&out, "  - %s:%d lang=%s\n", def.File, def.Line, def.Language)
			}
		}
	}
	return out.String(), nil
}

func (s *MCPServer) listFiles(pathFilter, pattern string, limit int) (string, error) {
	root := s.idx.Policy.WorkspaceRoot
	pathFilter = filepath.ToSlash(filepath.Clean(strings.TrimSpace(pathFilter)))
	if pathFilter == "." {
		pathFilter = ""
	}
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	files := make([]string, 0, limit)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if s.idx.Policy.ShouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(filepath.Clean(rel))
		if pathFilter != "" && !strings.HasPrefix(rel, pathFilter) {
			return nil
		}
		if pattern != "" && !strings.Contains(strings.ToLower(rel), pattern) {
			return nil
		}
		if _, ok := DetectOraLanguage(rel); !ok {
			return nil
		}
		files = append(files, rel)
		if len(files) >= limit {
			return errors.New("limit reached")
		}
		return nil
	})
	if err != nil && err.Error() != "limit reached" {
		return "", err
	}
	sort.Strings(files)
	var out strings.Builder
	fmt.Fprintf(&out, "## Files (%d)\n", len(files))
	for _, f := range files {
		fmt.Fprintf(&out, "- %s\n", f)
	}
	return out.String(), nil
}

func (s *MCPServer) findString(query, file, langFilter string, caseSensitive bool, limit int) (string, error) {
	type hit struct {
		File string
		Line int
		Text string
	}
	var hits []hit
	queryNeedle := query
	if !caseSensitive {
		queryNeedle = strings.ToLower(queryNeedle)
	}
	candidates := s.candidateFiles(file, langFilter)
	for _, rel := range candidates {
		abs, err := s.idx.Policy.ResolveWorkspacePath(rel)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		for i, line := range lines {
			hay := line
			if !caseSensitive {
				hay = strings.ToLower(hay)
			}
			if strings.Contains(hay, queryNeedle) {
				hits = append(hits, hit{File: rel, Line: i + 1, Text: strings.TrimSpace(line)})
				if len(hits) >= limit {
					goto done
				}
			}
		}
	}
done:
	var out strings.Builder
	fmt.Fprintf(&out, "## String Search `%s` (%d hits)\n", query, len(hits))
	if len(hits) == 0 {
		out.WriteString("No matches found.\n")
		return out.String(), nil
	}
	for _, h := range hits {
		fmt.Fprintf(&out, "- %s:%d | %s\n", h.File, h.Line, h.Text)
	}
	return out.String(), nil
}

func (s *MCPServer) candidateFiles(file, langFilter string) []string {
	if strings.TrimSpace(file) != "" {
		rel := filepath.ToSlash(filepath.Clean(file))
		return []string{rel}
	}
	var lang Language
	var filter bool
	if strings.TrimSpace(langFilter) != "" {
		if l, err := LanguageForName(langFilter); err == nil {
			lang = l
			filter = true
		}
	}
	s.idx.mu.RLock()
	candidates := make([]string, 0, len(s.idx.Files))
	for rel, meta := range s.idx.Files {
		if filter && meta != nil && meta.Language != lang {
			continue
		}
		candidates = append(candidates, filepath.ToSlash(filepath.Clean(rel)))
	}
	s.idx.mu.RUnlock()
	sort.Strings(candidates)
	return candidates
}

func (s *MCPServer) listRoutes(pathFilter string, limit int) (string, error) {
	routes, err := s.loadRouteEntries(pathFilter, limit)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "## Routes (%d)\n", len(routes))
	if len(routes) == 0 {
		out.WriteString("No route registrations detected.\n")
		return out.String(), nil
	}
	for _, r := range routes {
		if r.Handler != "" {
			fmt.Fprintf(&out, "- [%s] %s -> %s (%s:%d)\n", r.Method, r.Path, r.Handler, r.File, r.Line)
			if r.Resolved != "" && r.Resolved != r.Handler {
				fmt.Fprintf(&out, "  resolved: %s\n", r.Resolved)
			}
			continue
		}
		fmt.Fprintf(&out, "- [%s] %s (%s:%d)\n", r.Method, r.Path, r.File, r.Line)
	}
	return out.String(), nil
}

type routeEntry struct {
	File        string
	Line        int
	Method      string
	Path        string
	Handler     string
	Resolved    string
	Framework   string
	Confidence  string
	Middlewares []string
	CallGraph   []CallEdge
}

func (s *MCPServer) loadRouteEntries(pathFilter string, limit int) ([]routeEntry, error) {
	if s.idx != nil {
		if graph, err := s.idx.LoadRouteGraph(); err == nil && graph != nil {
			pathFilter = filepath.ToSlash(filepath.Clean(strings.TrimSpace(pathFilter)))
			if pathFilter == "." {
				pathFilter = ""
			}
			out := make([]routeEntry, 0, limit)
			for _, n := range graph.Routes {
				if pathFilter != "" && !strings.HasPrefix(n.File, pathFilter) {
					continue
				}
				out = append(out, routeEntry{
					File:        n.File,
					Line:        n.Line,
					Method:      n.Method,
					Path:        n.Path,
					Handler:     n.Handler,
					Resolved:    n.Resolved,
					Framework:   n.Framework,
					Confidence:  n.Confidence,
					Middlewares: append([]string{}, n.Middlewares...),
					CallGraph:   append([]CallEdge{}, n.CallGraph...),
				})
				if len(out) >= limit {
					break
				}
			}
			return out, nil
		}
	}
	return s.scanRoutes(pathFilter, limit)
}

func (s *MCPServer) scanRoutes(pathFilter string, limit int) ([]routeEntry, error) {
	pathFilter = filepath.ToSlash(filepath.Clean(strings.TrimSpace(pathFilter)))
	if pathFilter == "." {
		pathFilter = ""
	}
	methodCall := regexp.MustCompile(`\.(GET|POST|PUT|PATCH|DELETE|OPTIONS|HEAD|Get|Post|Put|Patch|Delete)\s*\(\s*"([^"]+)"(?:\s*,\s*([A-Za-z0-9_\.]+))?`)
	handleFunc := regexp.MustCompile(`\.(HandleFunc|Handle)\s*\(\s*"([^"]+)"(?:\s*,\s*([A-Za-z0-9_\.]+))?`)
	candidates := s.candidateFiles("", string(LanguageGo))
	routes := make([]routeEntry, 0, limit)
	for _, rel := range candidates {
		if pathFilter != "" && !strings.HasPrefix(rel, pathFilter) {
			continue
		}
		abs, err := s.idx.Policy.ResolveWorkspacePath(rel)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		fileMiddlewares := collectMiddlewares(lines)
		for i, line := range lines {
			if m := methodCall.FindStringSubmatch(line); len(m) > 0 {
				if !strings.HasPrefix(m[2], "/") {
					continue
				}
				method := strings.ToUpper(m[1])
				handler := strings.TrimSpace(m[3])
				routes = append(routes, routeEntry{
					File:        rel,
					Line:        i + 1,
					Method:      method,
					Path:        m[2],
					Handler:     handler,
					Framework:   "generic",
					Confidence:  "inferred",
					Middlewares: fileMiddlewares,
				})
			}
			if m := handleFunc.FindStringSubmatch(line); len(m) > 0 {
				if !strings.HasPrefix(m[2], "/") {
					continue
				}
				handler := strings.TrimSpace(m[3])
				routes = append(routes, routeEntry{
					File:        rel,
					Line:        i + 1,
					Method:      "ANY",
					Path:        m[2],
					Handler:     handler,
					Framework:   "generic",
					Confidence:  "inferred",
					Middlewares: fileMiddlewares,
				})
			}
			if len(routes) >= limit {
				break
			}
		}
		if len(routes) >= limit {
			break
		}
	}
	return routes, nil
}

func collectMiddlewares(lines []string) []string {
	useRe := regexp.MustCompile(`\.Use\(([^)]*)\)`)
	var mws []string
	for _, line := range lines {
		m := useRe.FindStringSubmatch(line)
		if len(m) < 2 {
			continue
		}
		parts := strings.Split(m[1], ",")
		for _, part := range parts {
			token := strings.TrimSpace(part)
			if token != "" {
				mws = append(mws, token)
			}
		}
	}
	return uniqueStrings(mws)
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func (s *MCPServer) traceRequest(urlPath, method string, limit int, depth int) (string, error) {
	method = strings.ToUpper(strings.TrimSpace(method))
	routes, err := s.loadRouteEntries("", 10000)
	if err != nil {
		return "", err
	}
	urlPath = strings.TrimSpace(urlPath)
	var matches []routeEntry
	for _, r := range routes {
		if method != "" && r.Method != "ANY" && r.Method != method {
			continue
		}
		if r.Path == urlPath {
			matches = append(matches, r)
			continue
		}
		if strings.HasPrefix(urlPath, r.Path) &&
			(r.Path == "/" || strings.HasSuffix(r.Path, "/") || urlPath[len(r.Path)] == '/') {
			matches = append(matches, r)
		}
		if len(matches) >= limit {
			break
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "## Request Trace %s %s (%d matches)\n", methodOrAny(method), urlPath, len(matches))
	if len(matches) == 0 {
		out.WriteString("No exact route match found.\n")
		return out.String(), nil
	}
	for _, r := range matches {
		fmt.Fprintf(&out, "- route: [%s] %s (%s:%d)\n", r.Method, r.Path, r.File, r.Line)
		if r.Handler != "" {
			fmt.Fprintf(&out, "  handler: %s\n", r.Handler)
		}
		if r.Resolved != "" && r.Resolved != r.Handler {
			fmt.Fprintf(&out, "  resolved: %s\n", r.Resolved)
		}
		if r.Framework != "" {
			fmt.Fprintf(&out, "  framework: %s\n", r.Framework)
		}
		if r.Confidence != "" {
			fmt.Fprintf(&out, "  confidence: %s\n", r.Confidence)
		}
		if len(r.Middlewares) > 0 {
			fmt.Fprintf(&out, "  middleware: %s\n", strings.Join(r.Middlewares, " -> "))
		}
		if depth > 0 && len(r.CallGraph) > 0 {
			fmt.Fprintf(&out, "  call chain (depth %d):\n", depth)
			rootFunc := r.Resolved
			if rootFunc == "" {
				rootFunc = r.Handler
			}
			printCallChain(r.CallGraph, rootFunc, 0, depth, &out)
		}
	}
	return out.String(), nil
}

func printCallChain(edges []CallEdge, start string, depth, maxDepth int, out *strings.Builder) {
	if depth >= maxDepth {
		return
	}
	localCallerMap := make(map[string][]CallEdge)
	for _, e := range edges {
		localCallerMap[e.Caller] = append(localCallerMap[e.Caller], e)
	}
	visited := make(map[string]bool)
	var printFn func(symbol string, currDepth int, indent string)
	printFn = func(symbol string, currDepth int, indent string) {
		if currDepth > maxDepth || visited[symbol] {
			return
		}
		visited[symbol] = true
		for _, e := range localCallerMap[symbol] {
			fmt.Fprintf(out, "%s%s (%s:%d) -> %s\n", indent, e.Caller, e.File, e.Line, e.Callee)
			printFn(e.Callee, currDepth+1, indent+"  ")
		}
		visited[symbol] = false
	}
	printFn(start, 1, "    ")
}

func methodOrAny(method string) string {
	if method == "" {
		return "ANY"
	}
	return method
}

func (s *MCPServer) describeTable(table string, limit int) (string, error) {
	table = strings.ToLower(strings.TrimSpace(table))
	sqlFiles := s.candidateFiles("", string(LanguageSQL))
	type tableDef struct {
		File    string
		Line    int
		Name    string
		Columns []string
	}
	var defs []tableDef
	createRe := regexp.MustCompile(`(?i)^\s*create\s+table\s+(if\s+not\s+exists\s+)?("?[\w\.]+"?)\s*\(`)
	for _, rel := range sqlFiles {
		abs, err := s.idx.Policy.ResolveWorkspacePath(rel)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		for i := 0; i < len(lines); i++ {
			line := lines[i]
			m := createRe.FindStringSubmatch(line)
			if len(m) < 3 {
				continue
			}
			name := strings.Trim(m[2], `"`)
			nameLower := strings.ToLower(name)
			if nameLower != table && !strings.HasSuffix(nameLower, "."+table) {
				continue
			}
			cols := make([]string, 0, 32)
			for j := i + 1; j < len(lines); j++ {
				t := strings.TrimSpace(lines[j])
				if t == "" || strings.HasPrefix(t, "--") {
					continue
				}
				if strings.HasPrefix(t, ")") {
					break
				}
				col := strings.TrimSuffix(t, ",")
				up := strings.ToUpper(col)
				if strings.HasPrefix(up, "CONSTRAINT ") || strings.HasPrefix(up, "PRIMARY KEY") || strings.HasPrefix(up, "FOREIGN KEY") || strings.HasPrefix(up, "UNIQUE ") || strings.HasPrefix(up, "CHECK ") {
					continue
				}
				fields := strings.Fields(col)
				if len(fields) < 2 {
					continue
				}
				colName := strings.Trim(fields[0], `"`)
				colType := strings.Join(fields[1:], " ")
				cols = append(cols, fmt.Sprintf("%s %s", colName, colType))
			}
			defs = append(defs, tableDef{File: rel, Line: i + 1, Name: name, Columns: cols})
			if len(defs) >= limit {
				break
			}
		}
		if len(defs) >= limit {
			break
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "## Table `%s` (%d definitions)\n", table, len(defs))
	if len(defs) == 0 {
		out.WriteString("No matching CREATE TABLE definition found.\n")
		return out.String(), nil
	}
	for _, d := range defs {
		fmt.Fprintf(&out, "- %s:%d table=%s\n", d.File, d.Line, d.Name)
		for _, c := range d.Columns {
			fmt.Fprintf(&out, "  - %s\n", c)
		}
	}
	return out.String(), nil
}

// ========== Effect helpers ==========
func (s *MCPServer) effectModality(name string) (string, error) {
	defs := s.idx.FindDefinitions(name)
	if len(defs) == 0 {
		return "", fmt.Errorf("symbol %q not found", name)
	}
	def := defs[0]
	if def.Language != LanguageGo {
		return "", fmt.Errorf("effect modality only supported for Go symbols")
	}
	mod := s.idx.GetEffectModality(name)
	if mod == nil {
		cst, err := s.idx.Pool.ParseFile(def.File, def.Language, s.idx.Policy)
		if err != nil {
			return "", fmt.Errorf("parse file: %w", err)
		}
		defer cst.Release()
		mod = extractModalitiesFromCST(cst, name)
		if mod == nil {
			return "", fmt.Errorf("could not locate function %q in %s", name, def.File)
		}
	}
	out := fmt.Sprintf("## Effect Modalities for `%s`\n", name)
	out += fmt.Sprintf("- error: %s\n", mod.Error)
	out += fmt.Sprintf("- io: %s\n", mod.IO)
	out += fmt.Sprintf("- panic: %s\n", mod.Panic)
	out += fmt.Sprintf("- async: %s\n", mod.Async)
	out += fmt.Sprintf("- security: %s\n", mod.Security)
	out += fmt.Sprintf("- resource: %s\n", mod.Resource)
	out += fmt.Sprintf("- confidence: %d%%\n", mod.Confidence)
	out += fmt.Sprintf("\nDefinition: %s:%d\n", def.File, def.Line)
	return out, nil
}

func (s *MCPServer) traceEffects(startSymbol string, maxDepth int) (string, error) {
	defs := s.idx.FindDefinitions(startSymbol)
	if len(defs) == 0 {
		return "", fmt.Errorf("symbol %q not found", startSymbol)
	}
	visited := map[string]bool{}
	type node struct {
		name  string
		depth int
	}
	queue := []node{{name: startSymbol, depth: 0}}
	var lines []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur.name] {
			continue
		}
		visited[cur.name] = true
		modStr, _ := s.effectModalityShort(cur.name)
		indent := strings.Repeat("  ", cur.depth)
		lines = append(lines, fmt.Sprintf("%s- %s [%s]", indent, cur.name, modStr))
		if cur.depth >= maxDepth {
			continue
		}
		refs := s.findRefs(cur.name)
		for _, ref := range refs {
			enclosing := s.findEnclosingFunction(ref.File, ref.Line)
			if enclosing != "" && !visited[enclosing] && enclosing != cur.name {
				queue = append(queue, node{name: enclosing, depth: cur.depth + 1})
			}
		}
	}
	if len(lines) == 0 {
		return fmt.Sprintf("No effect trace found for %s", startSymbol), nil
	}
	return "## Effect Trace\n" + strings.Join(lines, "\n"), nil
}

func (s *MCPServer) effectModalityShort(name string) (string, error) {
	defs := s.idx.FindDefinitions(name)
	if len(defs) == 0 {
		return "unknown", nil
	}
	def := defs[0]
	if def.Language != LanguageGo {
		return "unsupported", nil
	}
	mod := s.idx.GetEffectModality(name)
	if mod == nil {
		cst, err := s.idx.Pool.ParseFile(def.File, def.Language, s.idx.Policy)
		if err != nil {
			return "error", nil
		}
		defer cst.Release()
		mod = extractModalitiesFromCST(cst, name)
		if mod == nil {
			return "unknown", nil
		}
	}
	return fmt.Sprintf("err:%s io:%s panic:%s", mod.Error, mod.IO, mod.Panic), nil
}

func (s *MCPServer) findEnclosingFunction(file string, line int) string {
	syms := s.lookupFileSymbols(file)
	best := ""
	bestDist := -1
	for _, sym := range syms {
		if sym.Kind == "definition" && sym.Line <= line {
			dist := line - sym.Line
			if bestDist == -1 || dist < bestDist {
				bestDist = dist
				best = sym.Name
			}
		}
	}
	return best
}

func (s *MCPServer) resolveSymbolLocation(symbol string) (string, int, int, error) {
	defs := s.idx.FindDefinitions(symbol)
	if len(defs) == 0 {
		return "", 0, 0, fmt.Errorf("symbol %q not found", symbol)
	}
	def := defs[0]
	if def.Language == LanguageGo {
		body, err := s.ops.SymbolBody(def.File, symbol)
		if err == nil && body != "" {
			lines := strings.Split(body, "\n")
			absPath, absErr := s.idx.Policy.ResolveWorkspacePath(def.File)
			if absErr == nil {
				data, readErr := os.ReadFile(absPath)
				if readErr == nil {
					srcLines := strings.Split(string(data), "\n")
					braceLine := def.Line
					for braceLine < len(srcLines) {
						if strings.Contains(srcLines[braceLine-1], "{") {
							break
						}
						braceLine++
					}
					startLine := braceLine
					endLine := braceLine + len(lines) - 1
					if endLine > len(srcLines) {
						endLine = len(srcLines)
					}
					return def.File, startLine, endLine, nil
				}
			}
		}
	}
	return def.File, def.Line, def.Line + 20, nil
}

func (s *MCPServer) readSymbol(name string) (string, error) {
	defs := s.idx.FindDefinitions(name)
	if len(defs) == 0 {
		return "", fmt.Errorf("symbol %q not found", name)
	}
	def := defs[0]
	text, err := s.sourceRange(def.File, def.Line, def.Line+200)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("## Symbol `%s` at %s:%d\n%s", name, def.File, def.Line, text), nil
}

// ========== Edit rendering helpers ==========
func renderEditEntries(title string, edits []EditEntry) string {
	if len(edits) == 0 {
		return "No edit history found."
	}
	var out strings.Builder
	fmt.Fprintf(&out, "## %s (%d)\n", title, len(edits))
	for _, e := range edits {
		fmt.Fprintf(&out, "- %s | %s\n", e.Timestamp, e.Task)
		if e.Reason != "" {
			fmt.Fprintf(&out, "  reason: %s\n", e.Reason)
		}
		if len(e.Files) > 0 {
			fmt.Fprintf(&out, "  files: %s\n", strings.Join(e.Files, ", "))
		}
		if len(e.Symbols) > 0 {
			fmt.Fprintf(&out, "  symbols: %s\n", strings.Join(e.Symbols, ", "))
		}
		if e.Verification != "" {
			fmt.Fprintf(&out, "  verify: %s\n", e.Verification)
		}
		if e.Decision != "" {
			fmt.Fprintf(&out, "  decision: %s\n", e.Decision)
		}
	}
	return out.String()
}

func renderModuleInfo(file string, info ModuleInfo) string {
	var out strings.Builder
	fmt.Fprintf(&out, "## Module Owner for `%s`\n", file)
	fmt.Fprintf(&out, "- module: %s\n", info.Name)
	if len(info.Owns) > 0 {
		fmt.Fprintf(&out, "- owns: %s\n", strings.Join(info.Owns, ", "))
	}
	if len(info.Frontend) > 0 {
		fmt.Fprintf(&out, "- frontend: %s\n", strings.Join(info.Frontend, ", "))
	}
	if len(info.Backend) > 0 {
		fmt.Fprintf(&out, "- backend: %s\n", strings.Join(info.Backend, ", "))
	}
	return out.String()
}

func parseEditEntryArgs(args map[string]interface{}) (EditEntry, error) {
	task, _ := args["task"].(string)
	if strings.TrimSpace(task) == "" {
		return EditEntry{}, fmt.Errorf("task is required")
	}
	filesRaw, _ := args["files"].([]interface{})
	files := make([]string, 0, len(filesRaw))
	for _, f := range filesRaw {
		if s, ok := f.(string); ok && strings.TrimSpace(s) != "" {
			files = append(files, strings.TrimSpace(s))
		}
	}
	if len(files) == 0 {
		return EditEntry{}, fmt.Errorf("files is required")
	}
	reason, _ := args["reason"].(string)
	verification, _ := args["verification"].(string)
	decision, _ := args["decision"].(string)
	symbolsRaw, _ := args["symbols"].([]interface{})
	symbols := make([]string, 0, len(symbolsRaw))
	for _, s := range symbolsRaw {
		if str, ok := s.(string); ok && strings.TrimSpace(str) != "" {
			symbols = append(symbols, strings.TrimSpace(str))
		}
	}
	return EditEntry{
		Timestamp:    "",
		Task:         strings.TrimSpace(task),
		Reason:       strings.TrimSpace(reason),
		Files:        files,
		Symbols:      symbols,
		Verification: strings.TrimSpace(verification),
		Decision:     strings.TrimSpace(decision),
	}, nil
}

func extractBindingSurface(vueSource []byte) string {
	blocks, _ := ParseVueSFC(bytes.NewReader(vueSource))
	var templateContent string
	for _, b := range blocks {
		if b.Tag == "template" {
			templateContent = b.Content
			break
		}
	}
	if templateContent == "" {
		return "No template block found"
	}
	re := regexp.MustCompile(`\{\{\s*(\w+)\s*\}\}|v-model\s*=\s*["'](\w+)["']|:[\w-]+\s*=\s*["'](\w+)["']`)
	matches := re.FindAllStringSubmatch(templateContent, -1)
	vars := make(map[string]bool)
	for _, m := range matches {
		for i := 1; i < len(m); i++ {
			if m[i] != "" {
				vars[m[i]] = true
			}
		}
	}
	var out []string
	for v := range vars {
		out = append(out, v)
	}
	return strings.Join(out, ", ")
}

// --- Watchdog Helpers ---
func (s *MCPServer) fetchWatchdogArtifact(artifact string) (string, error) {
	resp, err := http.Get(fmt.Sprintf("http://localhost:9191/artifacts/%s", artifact))
	if err != nil { return "", fmt.Errorf("watchdog API error: %w", err) }
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil { return "", err }
	return string(b), nil
}

func (s *MCPServer) postWatchdogSupervisor(service, action string) (string, error) {
	req, err := http.NewRequest("POST", fmt.Sprintf("http://localhost:9191/api/supervisor/%s/%s", url.PathEscape(service), action), nil)
	if err != nil { return "", err }
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return "", fmt.Errorf("watchdog API error: %w", err) }
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil { return "", err }
	return string(b), nil
}

func (s *MCPServer) fetchWatchdogLogs(service string, lines int) (string, error) {
	resp, err := http.Get(fmt.Sprintf("http://localhost:9191/api/logs?service=%s&lines=%d", url.QueryEscape(service), lines))
	if err != nil { return "", fmt.Errorf("watchdog API error: %w", err) }
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil { return "", err }
	return string(b), nil
}

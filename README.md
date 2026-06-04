# OraCode

OraCode is a syntax-aware code intelligence engine for Go, Vue, TypeScript, JavaScript, and SQL workspaces.
It runs as a **stdio MCP server**, exposing tools that let LLM agents read, search, trace, and validate code without ever calling grep or reading entire files.

---

## Why OraCode

- **No grep, no file dumps.** Tools return only the lines or symbols you ask for.
- **Surgical reads.** Read a single symbol body, a line range, or a route handler chain — not an entire file.
- **Persistent memory.** Every edit and architecture decision is stored so agents can resume without losing context.
- **Contract enforcement.** Define what a module is allowed to do; OraCode detects when the code breaks that contract.
- **Multi-language.** Go, Vue SFC, TypeScript, JavaScript, SQL, and protobuf are all indexed together.

---

## Quick Start

### Build

```powershell
cd C:\path\to\oracode_project
go build -o .\tools\oracode.exe .
```

### Run as MCP server

```powershell
.\tools\oracode.exe -mcp -workspace C:\path\to\your\repo
```

Add `-effects` to also build the effect graph (required for `scalpel_effect_modality`, `scalpel_trace_effects`, `scalpel_find_violations`):

```powershell
.\tools\oracode.exe -mcp -workspace C:\path\to\your\repo -effects
```

### MCP config (paste into your tool's settings)

```json
{
  "mcpServers": {
    "oracode": {
      "command": "C:\\path\\to\\oracode.exe",
      "args": ["-mcp", "-workspace", "C:\\path\\to\\your\\repo"]
    }
  }
}
```

For VS Code / Cursor use `"mcp.servers"` as the key instead of `"mcpServers"`. The `command` and `args` values are identical.

### Generated state and local cache

OraCode keeps generated artifacts in a local cache folder keyed to the workspace path, so a workspace that lives in OneDrive still behaves like a normal writable local project.

By default the cache lives under your user cache directory, for example:

```text
C:\Users\<you>\AppData\Local\oracode\<workspace-hash>\
```

Set `ORACODE_STATE_DIR` if you want the cache somewhere else:

```powershell
$env:ORACODE_STATE_DIR = "C:\OraCodeState"
.\tools\oracode.exe -mcp -workspace C:\path\to\your\repo
```

The repo's `.oracode/` folder can still hold source-controlled docs like contracts or notes, but the generated graphs, logs, and caches are written to the local state directory.

---

## Defaults and Safety Limits

| Setting | Default |
|---|---|
| Max file size indexed | 2 MB |
| Index workers | 6 |
| Generated state | Local cache, not workspace `.oracode` |
| Workspace path escape | Blocked |
| Symlink escape | Blocked |
| Skipped folders | `.git`, `node_modules`, `dist`, `vendor`, `__pycache__`, `.oracode` |

---

## MCP Tools Reference

Tools are grouped by what they do. Each entry shows: **what it does**, **required args**, **optional args**, and a ready-to-use call example.

---

### Symbol Tools

Use these to find, read, and understand code symbols (functions, structs, types, variables).

---

#### `scalpel_find_symbol`

Find where a symbol is defined in the workspace.

| Arg | Type | Required | Description |
|---|---|---|---|
| `name` | string | ✅ | Symbol name to find |
| `lang` | string | | Filter by language (`go`, `vue`, `ts`, etc.) |
| `kind` | string | | Filter by kind (`definition`, `reference`) |

```json
{"name": "scalpel_find_symbol", "arguments": {"name": "CreateOrder"}}
```

---

#### `scalpel_find_refs`

Find every place a symbol is called or used.

| Arg | Type | Required | Description |
|---|---|---|---|
| `name` | string | ✅ | Symbol name to find references for |

```json
{"name": "scalpel_find_refs", "arguments": {"name": "CreateOrder"}}
```

---

#### `scalpel_get_symbol_context`

Return the source lines around a symbol's definition. Useful before editing.

| Arg | Type | Required | Description |
|---|---|---|---|
| `name` | string | ✅ | Symbol name |
| `contextLines` | integer | | Lines of context above/below (default 8, max 40) |
| `includeRefs` | boolean | | Also list all reference sites |

```json
{"name": "scalpel_get_symbol_context", "arguments": {"name": "CreateOrder", "contextLines": 10}}
```

---

#### `scalpel_read_symbol`

Read only the body of a function or definition — not the whole file. For Go, uses the AST to extract precise bounds.

| Arg | Type | Required | Description |
|---|---|---|---|
| `name` | string | ✅ | Symbol name |

```json
{"name": "scalpel_read_symbol", "arguments": {"name": "ProcessPayment"}}
```

---

#### `scalpel_get_range`

Read a specific line range from a file with line numbers.

| Arg | Type | Required | Description |
|---|---|---|---|
| `file` | string | ✅ | Relative file path |
| `start` | integer | ✅ | Start line (1-indexed) |
| `end` | integer | | End line (defaults to start, max 240 lines) |

```json
{"name": "scalpel_get_range", "arguments": {"file": "api/internal/sales/handler.go", "start": 42, "end": 80}}
```

---

#### `scalpel_get_file_symbols`

List every definition and reference indexed for a file.

| Arg | Type | Required | Description |
|---|---|---|---|
| `file` | string | ✅ | Relative file path |

```json
{"name": "scalpel_get_file_symbols", "arguments": {"file": "api/internal/sales/service.go"}}
```

---

#### `scalpel_dependencies`

List all symbols referenced by a file (its dependencies). Can also look up by symbol name.

| Arg | Type | Required | Description |
|---|---|---|---|
| `file` | string | | Relative file path |
| `symbol` | string | | Symbol name (used to find the file if `file` is omitted) |

```json
{"name": "scalpel_dependencies", "arguments": {"file": "api/internal/sales/service.go"}}
```

---

#### `scalpel_list_imports`

List all import paths declared in a file.

| Arg | Type | Required | Description |
|---|---|---|---|
| `file` | string | ✅ | Relative file path |

```json
{"name": "scalpel_list_imports", "arguments": {"file": "api/internal/sales/service.go"}}
```

---

#### `scalpel_reindex`

Re-index a single file after you edit it, or re-index the entire workspace. Call this after making code changes so other tools see the updated symbols.

| Arg | Type | Required | Description |
|---|---|---|---|
| `file` | string | | Relative file path. Omit to reindex the whole workspace. |

```json
{"name": "scalpel_reindex", "arguments": {"file": "api/internal/sales/service.go"}}
```

---

### Search & Discovery

Use these to find files, text, routes, and database tables without knowing the exact symbol name.

---

#### `scalpel_list_files`

List all indexed source files. Filter by directory path or filename pattern.

| Arg | Type | Required | Description |
|---|---|---|---|
| `path` | string | | Directory prefix to filter by (e.g. `api/internal`) |
| `pattern` | string | | Substring to match in filename (e.g. `sales`) |
| `limit` | integer | | Max results (default 100, max 1000) |

```json
{"name": "scalpel_list_files", "arguments": {"path": "api/internal/sales", "limit": 50}}
```

---

#### `scalpel_find_string`

Search for any string or substring across all indexed source files. Faster than grep for agents because no shell is needed.

| Arg | Type | Required | Description |
|---|---|---|---|
| `query` | string | ✅ | Text to search for |
| `file` | string | | Restrict search to one file |
| `lang` | string | | Restrict search to one language (`go`, `vue`, `sql`, etc.) |
| `caseSensitive` | boolean | | Default false |
| `limit` | integer | | Max results (default 50, max 500) |

```json
{"name": "scalpel_find_string", "arguments": {"query": "tenant not found", "lang": "go", "limit": 30}}
```

---

#### `scalpel_list_routes`

List all HTTP route registrations found in Go files (gin, chi, echo, net/http).

| Arg | Type | Required | Description |
|---|---|---|---|
| `path` | string | | Directory prefix to filter (e.g. `api/internal/sales`) |
| `limit` | integer | | Max results (default 200, max 2000) |

```json
{"name": "scalpel_list_routes", "arguments": {"path": "api/internal/sales"}}
```

---

#### `scalpel_trace_request`

Given a URL path and HTTP method, return the matching route, handler, middleware chain, and framework.

| Arg | Type | Required | Description |
|---|---|---|---|
| `url` | string | ✅ | URL path (e.g. `/api/sales/invoice`) |
| `method` | string | | HTTP method (GET, POST, etc.). Omit to match any. |
| `limit` | integer | | Max matches (default 20) |

```json
{"name": "scalpel_trace_request", "arguments": {"url": "/api/sales/invoice", "method": "POST"}}
```

---

#### `scalpel_list_tables`

List every SQL table definition indexed in the workspace.

```json
{"name": "scalpel_list_tables", "arguments": {}}
```

---

#### `scalpel_describe_table`

Show the columns and types for a specific SQL table, extracted from its `CREATE TABLE` definition.

| Arg | Type | Required | Description |
|---|---|---|---|
| `table` | string | ✅ | Table name (case-insensitive) |
| `limit` | integer | | Max matching definitions to return (default 20) |

```json
{"name": "scalpel_describe_table", "arguments": {"table": "sales_orders"}}
```

---


#### scalpel_semantic_search (Multi-Store Domain Retrieval)

OraCode now segments its vector database into **domain-specific knowledge stores**. Instead of searching a monolithic index where code, API routes, and meeting notes are jumbled together, you can query specialized layers of your architecture. This drastically reduces noise and improves LLM accuracy.

| Arg | Type | Required | Description |
|---|---|---|---|
| query | string | ✅ | Natural language query |
| store | string | | Domain store to query: 'code' (default), 'adr' (architecture decisions), 'route' (API endpoints), 'domain' (domain models), 'schema' (SQL tables) |
| limit | integer | | Max results (default 5) |

```json
{
  "name": "scalpel_semantic_search",
  "arguments": {
    "query": "What is the decided approach for cross-module communication in inventory?",
    "store": "adr",
    "limit": 3
  }
}
```
### Surface Inspection

Use these to get a structured overview of a module's SQL schema, protobuf services, or Vue components without reading individual files.

---

#### `scalpel_schema_surface`

Return the SQL schema (tables and columns) owned by a module from the pre-built SQL graph.

| Arg | Type | Required | Description |
|---|---|---|---|
| `module` | string | ✅ | Module name as defined in `module_map.yaml` |

```json
{"name": "scalpel_schema_surface", "arguments": {"module": "sales"}}
```

---

#### `scalpel_proto_surface`

Return the gRPC service definition (methods, request/response types) from the pre-built proto graph.

| Arg | Type | Required | Description |
|---|---|---|---|
| `service` | string | ✅ | gRPC service name |

```json
{"name": "scalpel_proto_surface", "arguments": {"service": "SalesService"}}
```

---

#### `scalpel_vue_surface`

Return the Vue component list (bindings, props, emits) for a module from the pre-built Vue surface graph.

| Arg | Type | Required | Description |
|---|---|---|---|
| `module` | string | ✅ | Module name |

```json
{"name": "scalpel_vue_surface", "arguments": {"module": "sales"}}
```

---

#### `scalpel_sfc_read_block`

Read a single block (`script`, `template`, or `style`) from a Vue Single File Component. Also returns the template binding surface when reading `script`.

| Arg | Type | Required | Description |
|---|---|---|---|
| `file` | string | ✅ | Absolute or relative path to the `.vue` file |
| `block` | string | ✅ | Block type: `script`, `template`, or `style` |

```json
{"name": "scalpel_sfc_read_block", "arguments": {"file": "frontend/src/views/sales/InvoiceForm.vue", "block": "script"}}
```

---

### Effect Analysis

OraCode infers **effect modalities** for Go functions — whether they can error, do I/O, panic, run async code, touch security-sensitive paths, or hold resources. This requires the server to be started with `-effects`.

Modality values: `will` | `can` | `might` | `wont` | `must` | `unknown`

---

#### `scalpel_effect_modality`

Return the effect modalities for a single Go symbol.

| Arg | Type | Required | Description |
|---|---|---|---|
| `name` | string | ✅ | Symbol name |

```json
{"name": "scalpel_effect_modality", "arguments": {"name": "ProcessPayment"}}
```

Example response:
```
## Effect Modalities for `ProcessPayment`
- error: will
- io: will
- panic: might
- async: wont
- security: must
- resource: can
- confidence: 85%
```

---

#### `scalpel_trace_effects`

Trace effect propagation up the call chain from a symbol. Shows every caller and their combined modalities up to a given depth.

| Arg | Type | Required | Description |
|---|---|---|---|
| `name` | string | ✅ | Starting symbol |
| `depth` | integer | | How many hops to trace (default 3) |

```json
{"name": "scalpel_trace_effects", "arguments": {"name": "ProcessPayment", "depth": 2}}
```

---

#### `scalpel_graph_version`

Return the version number and build timestamp of the current effect graph. Use to check if the graph is stale.

```json
{"name": "scalpel_graph_version", "arguments": {}}
```

---

### Validation & Contracts

Use these to enforce architecture rules and check that a module's code matches its declared contract.

---

#### `scalpel_validate_code`

Check a code snippet against all constraints defined in `.oracode/constraints.yaml`. Returns a list of violations or "No violations".

Constraints are regex or AST-based rules (e.g. "all Go DB calls must have error handling", "no raw SQL in handlers").

| Arg | Type | Required | Description |
|---|---|---|---|
| `code` | string | ✅ | Source code to validate |
| `language` | string | ✅ | Language of the snippet (`go`, `ts`, `vue`, etc.) |

```json
{
  "name": "scalpel_validate_code",
  "arguments": {
    "code": "func handler(c *gin.Context) { db.Exec(query) }",
    "language": "go"
  }
}
```

---

#### `scalpel_module_contracts`

Read the declared contract for a module from `.oracode/module_contracts/<module>.json`. The contract lists the module's exported symbols and their expected effect modalities.

| Arg | Type | Required | Description |
|---|---|---|---|
| `module` | string | ✅ | Module name (maps to `.oracode/module_contracts/<module>.json`) |

```json
{"name": "scalpel_module_contracts", "arguments": {"module": "sales"}}
```

---

#### `scalpel_find_violations`

Compare the module's contract (expected modalities) against the live effect graph (actual modalities). Reports every symbol where the actual behaviour deviates from the contract.

| Arg | Type | Required | Description |
|---|---|---|---|
| `module` | string | ✅ | Module name |

```json
{"name": "scalpel_find_violations", "arguments": {"module": "sales"}}
```

Example violation output:
```
## Contract Violations
ProcessPayment: expected error:wont, actual:will
CreateInvoice: expected io:wont, actual:will
```

---

#### scalpel_context_packet

Assemble everything an agent needs to start working on a task into a single response.
**IMPORTANT:** This tool is strictly for retrieving context about *known modules and symbols* (it fetches effect modalities, SQL tables, gRPC services, HTTP routes, architectural constraints, and recent architecture decisions). Do **NOT** use this tool to search for new tasks or unfamiliar concepts. To discover where code lives or search for tasks, use `scalpel_semantic_search` or `scalpel_find_string` instead.

Call this **at the start of a task** to orient yourself without making a dozen separate calls.

| Arg | Type | Required | Description |
|---|---|---|---|
| `task` | string | ✅ | Short description of the task (used as a label) |
| `module` | string | ✅ | Module to gather context for |

```json
{"name": "scalpel_context_packet", "arguments": {"task": "Add discount support to sales invoices", "module": "sales"}}
```

---

### Surgical Edit Tools

Use these when you already know the target symbol or route and want OraCode to perform a tight, traceable edit instead of a broad refactor.

#### `scalpel_symbol_signature`

Return a compact declaration signature for a Go symbol without loading the whole file body.

```json
{"name": "scalpel_symbol_signature", "arguments": {"file": "oracode/mcp_server.go", "name": "NewMCPServer"}}
```

#### `scalpel_symbol_body`

Return only the body of a Go function or declaration body when you need the exact implementation before editing.

```json
{"name": "scalpel_symbol_body", "arguments": {"file": "oracode/chain_tracer.go", "name": "traceChain"}}
```

#### `scalpel_replace_symbol`

Replace a Go declaration surgically. Use this for a function, type, const block, or variable block when you want a full declaration swap.

```json
{
  "name": "scalpel_replace_symbol",
  "arguments": {
    "file": "oracode/service.go",
    "name": "BuildContextPacket",
    "replacement": "func BuildContextPacket(task string) string { return task }"
  }
}
```

#### `scalpel_add_struct_field`

Add a field to an existing struct without rewriting the whole type.

```json
{
  "name": "scalpel_add_struct_field",
  "arguments": {
    "file": "oracode/config.go",
    "structName": "Config",
    "fieldSrc": "StateDir string `json:\"state_dir\"`"
  }
}
```

#### `scalpel_remove_struct_field`

Remove a field from a struct when a property is deprecated or duplicated.

```json
{
  "name": "scalpel_remove_struct_field",
  "arguments": {
    "file": "oracode/config.go",
    "structName": "Config",
    "fieldName": "LegacyPath"
  }
}
```

#### `scalpel_trace_chain`

Trace an HTTP endpoint from route registration to handler calls and SQL tables. This is the high-signal debugging tool for 403/404s, missing tables, and request flow questions.

```json
{
  "name": "scalpel_trace_chain",
  "arguments": {
    "url": "/api/orders/submit",
    "method": "POST",
    "depth": 3
  }
}
```

Recommended usage:
1. Find the route or symbol.
2. Read only the signature or body you need.
3. Apply one surgical edit.
4. Reindex the changed file.
5. Record the edit so the next session does not have to rediscover it.

---

### High-Leverage Surgical Tools

These are the next tools to reach for when you want OraCode to stay small at the top level but still solve hard refactor and debugging tasks.

#### `scalpel_rename_symbol`

Rename a Go symbol across definitions and references inside a scope.

```json
{"name": "scalpel_rename_symbol", "arguments": {"old_name": "GetPOContext", "new_name": "GetPrintContext", "scope": "internal/purchasing"}}
```

#### `scalpel_schema_gap`

Compare the indexed SQL schema with the live PostgreSQL schema via `ORACODE_DB_DSN`.

```json
{"name": "scalpel_schema_gap", "arguments": {}}
```

#### `scalpel_trace_error`

Trace a watchdog error reference into endpoint, call chain, and schema context.

```json
{"name": "scalpel_trace_error", "arguments": {"ref": "ERR-E401UZ"}}
```

#### `scalpel_module_summary`

Return a one-page overview of a module with routes, tables, edits, and decisions.

```json
{"name": "scalpel_module_summary", "arguments": {"module": "oracode"}}
```

#### `scalpel_blast_radius`

Show callers, routes, frontend usage, and tables touched by a symbol.

```json
{"name": "scalpel_blast_radius", "arguments": {"symbol": "NewMCPServer", "depth": 2}}
```

#### `scalpel_apply_pattern`

Apply a capture-based pattern replacement inside a scoped folder, with optional dry-run preview.

```json
{"name": "scalpel_apply_pattern", "arguments": {"lang": "go", "pattern": "fmt.Errorf($MSG)", "replacement": "fmt.Errorf($MSG: %w, err)", "scope": "oracode", "dry_run": true}}
```

---

### Memory & Governance

OraCode persists edit history and architecture decisions in a local state cache keyed to the workspace, so agents can resume work with full context even when the repo itself lives on OneDrive or another synced volume.

| File | Contents |
|---|---|
| local cache `edit_log.jsonl` | Structured log of every recorded edit |
| local cache `decisions.md` | Freeform architecture decision log |
| local cache `module_map.yaml` | Maps file paths to owning modules |
| `.oracode/constraints.yaml` | Architectural constraint rules kept in the workspace when you want them source-controlled |
| `.oracode/module_contracts/` | Per-module contract JSON files |
| local cache `confidence_store.json` | Learned confidence scores for effect patterns |

---

#### `scalpel_recent_edits`

Return the most recent recorded edits across the whole workspace.

| Arg | Type | Required | Description |
|---|---|---|---|
| `limit` | integer | | Number of edits to return (default 10, max 100) |

```json
{"name": "scalpel_recent_edits", "arguments": {"limit": 20}}
```

---

#### `scalpel_edits_for_file`

Return recorded edits that touched a specific file.

| Arg | Type | Required | Description |
|---|---|---|---|
| `file` | string | ✅ | Relative file path |
| `limit` | integer | | Max results (default 10) |

```json
{"name": "scalpel_edits_for_file", "arguments": {"file": "api/internal/sales/service.go"}}
```

---

#### `scalpel_edits_for_symbol`

Return recorded edits that mention a specific symbol.

| Arg | Type | Required | Description |
|---|---|---|---|
| `name` | string | ✅ | Symbol name |
| `limit` | integer | | Max results (default 10) |

```json
{"name": "scalpel_edits_for_symbol", "arguments": {"name": "CreateInvoice"}}
```

---

#### `scalpel_record_edit`

Write a structured edit record to `edit_log.jsonl`. Call this **after every change** so future agents have context.

If you include a `pattern` and `confidence` value, OraCode also updates its confidence store so modality inference improves over time.

| Arg | Type | Required | Description |
|---|---|---|---|
| `task` | string | ✅ | What you were doing |
| `files` | string[] | ✅ | Files changed |
| `reason` | string | | Why you made the change |
| `symbols` | string[] | | Key symbols touched |
| `verification` | string | | How you verified the change (e.g. `go test ./...`) |
| `decision` | string | | Any architecture decision made |
| `pattern` | string | | Effect pattern learned (e.g. `func (*sql.Tx) -> must:rollback`) |
| `confidence` | integer | | Confidence score 0–100 for the pattern |

```json
{
  "name": "scalpel_record_edit",
  "arguments": {
    "task": "Add discount field to sales invoices",
    "reason": "Business requirement: tiered customer discounts",
    "files": ["api/internal/sales/service.go", "api/internal/sales/model.go"],
    "symbols": ["CreateInvoice", "Invoice"],
    "verification": "go test ./api/internal/sales/... passed",
    "decision": "Discount is stored as basis points to avoid float precision issues"
  }
}
```

---

#### `scalpel_decision_log`

Return the latest architecture decisions from `decisions.md`.

| Arg | Type | Required | Description |
|---|---|---|---|
| `limit` | integer | | Max lines to return (default 80, max 400) |

```json
{"name": "scalpel_decision_log", "arguments": {"limit": 40}}
```

---

#### `scalpel_module_owner`

Look up which module owns a given file path, using `module_map.yaml`.

| Arg | Type | Required | Description |
|---|---|---|---|
| `file` | string | ✅ | Relative file path |

```json
{"name": "scalpel_module_owner", "arguments": {"file": "api/internal/sales/service.go"}}
```

---

## Recommended Agent Workflow

Follow this loop for any code edit task:

```
1. scalpel_context_packet        → get full module context upfront
2. scalpel_find_symbol           → locate the symbol to change
3. scalpel_read_symbol           → read its body precisely
4. scalpel_find_refs             → check what calls it before touching it
5. scalpel_get_symbol_context    → read surrounding lines if needed
6. [make your edit]
7. scalpel_reindex               → update the index for the changed file
8. scalpel_validate_code         → check the new code passes constraints
9. scalpel_find_violations       → confirm contract is still satisfied
10. scalpel_record_edit          → log the change with task, reason, verification
```

---

## Troubleshooting

**Symbol not found**
Run `scalpel_reindex` for the file. Confirm the file extension is a supported language (`.go`, `.ts`, `.tsx`, `.js`, `.vue`, `.sql`).

**Route tracing looks incomplete**
Run a full `scalpel_reindex` to rebuild the route graph. Confirm router registrations use a supported call shape (`.GET(`, `.POST(`, `.HandleFunc(`).

**Effect tools return nothing / effect graph not available**
Start the MCP server with the `-effects` flag. Then run `scalpel_reindex` to populate the graph.

**`scalpel_find_violations` returns "effect graph not available"**
Same as above — the `-effects` flag is required.

**`scalpel_module_contracts` returns "contract not found"**
Create `.oracode/module_contracts/<module>.json` with the contract definition. See `scalpel_find_violations` for the expected JSON shape.

**Large SQL files skipped**
The default cap is 2 MB. Increase `maxSQLFileBytes` in `security.go` if needed.

**MCP not connecting**
Check the `command` path is absolute and the exe exists. Confirm `workspace` path exists. Check whether your host expects `mcpServers` or `mcp.servers` as the config key.

---

## Project Layout

```
oracode_project/
├── main.go                      # Entry point: -mcp, -serve, -effects flags
├── go.mod
└── oracode/
    ├── mcp_server.go            # All MCP tool definitions and handlers
    ├── index.go                 # Symbol index with RWMutex
    ├── effect.go                # Effect modality extraction
    ├── effect_graph.go          # Effect graph data structures
    ├── constraint.go            # Constraint loading and ValidateCode
    ├── confidence.go            # Confidence store (learns from recorded edits)
    ├── memory.go                # Edit log and decision log (EditStore)
    ├── route_graph.go           # Route graph build and query
    ├── sql_graph.go             # SQL schema surface graph
    ├── proto_graph.go           # Protobuf surface graph
    ├── vue_surface.go           # Vue component surface graph
    ├── sfc_parser.go            # Vue SFC block parser
    ├── extractor.go             # Tree-sitter symbol extraction
    ├── parser_pool.go           # Parser pool (memory bounded)
    ├── security.go              # Path sanitisation, skip rules, file size limits
    ├── watcher.go               # File watcher for incremental reindex
    ├── server.go                # REST server (optional, -serve flag)
    └── engine.go                # CLI refactor engine
```

---

## Roadmap

- Multi-hop handler resolution for route tracing
- `go/packages` semantic graph for cross-package type resolution
- Ruleguard/ast-grep policy packs for constraint rules
- Watchdog error stream integration (runtime error → symbol correlation)
#   o r a c o d e  
 
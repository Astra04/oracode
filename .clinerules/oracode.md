# OraCode Agent Protocol

This repository is powered by **OraCode**, an in-memory, syntax-aware code intelligence engine.

As an agent operating in this repository, you must **never use generic shell tools like grep, cat, or sed**. You must strictly use the MCP scalpel_* toolset provided.

---

## 🏗️ The 4-Phase Master Edit Protocol

Every single code modification MUST follow this strict 4-phase sequence. No exceptions.

### Phase 1: DISCOVER
Assemble a high-density, targeted context payload of the module you are working in.
* **Primary Tool:** scalpel_context_packet (returns tables, routes, contracts, and ADR decisions)
* **When lost:** If you don't know the exact symbol name or you need to understand intent, use scalpel_semantic_search or scalpel_find_string.
* **Identify Ownership:** Use scalpel_module_owner or module map boundaries.

### Phase 2: INSPECT
Do not read whole files. Target exactly what you need to modify.
* **Primary Tool:** scalpel_inspect_symbol (use mode: "body" to read functions, mode: "refs" to find callers)
* **For Vue files:** Read specific files to isolate templates or scripts.
* **Plan:** Build your mental model of the required edits.

### Phase 3: EXECUTE (The batch_edit Pipeline)
**Standalone edit tools do not exist in this workspace.** ALL writes must go through scalpel_batch_edit.

1. **Construct Patch Array:** Select the exact mutation_type from the table below.
2. **Preview (Mandatory):** Submit scalpel_batch_edit with dry_run: true.
3. **Review:** Read the DryRunValidation output. The server will reject structural errors.
4. **Commit:** Resubmit scalpel_batch_edit with dry_run: false within 10 minutes. If AST compilation or CSS validation fails, the server will automatically roll back your edits.

### Phase 4: GOVERN
Ensure architectural stability and persist agent memory.
* **Primary Tool:** scalpel_record_edit
* **Rule:** If you made an architectural decision, immediately invoke scalpel_log_decision.

---

## 🛠️ Mutation Type Quick-Reference

When calling scalpel_batch_edit, your patches array elements MUST conform exactly to these schemas:

| mutation_type | Language | Required Fields | Usage Rules & Notes |
|---|---|---|---|
| replace_symbol | Go | symbol_anchor, new_content | Replaces the entire AST node for a func/type. Use this instead of range_replace whenever you know the symbol name. |
| add_struct_field | Go | symbol_anchor, new_content | Safely appends a field inside a struct block. |
| range_replace | Any | start_line, end_line, old_content, new_content | Splicing lines. **Always requires old_content**. For Vue files, add block ("template", "script") to use lines *relative* to that block. |
| replace_symbol_vue | Vue/TS | symbol_anchor, new_content | Replaces a ref, function, or const in a script block. |
| replace_block | Vue | block, new_content | Replaces the entire content of a Vue block (e.g., style). |
| vue_inject_directive | Vue | tag, directive | Optional: match_attr. Safely injects v-if etc into tags. |
| add_composable_vue | Vue | new_content | Safely injects a composable call below imports in script setup. |
| add_import_vue | Vue | new_content | e.g. import { ref } from 'vue' |

**CRITICAL RULE:** range_replace on .go files is blocked if you provide a symbol_anchor. You must use replace_symbol to ensure safe AST replacement.

---

## 🛑 Guardrails
1. **Never skip dry_run: true**: The server enforces a ledger; a write will fail if not previewed.
2. **Never hallucinate schema**: Read the table above.
3. **Handle Rollbacks**: If batch_edit fails with "Syntax error detected by tree-sitter parser" or a CSS violation, your code was rolled back. Fix the missing brace or semicolon and resubmit.

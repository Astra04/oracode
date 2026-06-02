package oracode

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// BatchPatch describes one atomic change in a batch edit.
type BatchPatch struct {
	Group        string `json:"group"`                   // group name, default ""
	File         string `json:"file"`                    // relative workspace path
	SymbolAnchor string `json:"symbol_anchor,omitempty"` // for symbol-targeted patches
	Block        string `json:"block,omitempty"`         // for Vue: "script", "template", "style"

	// Range (for range_replace)
	StartLine int `json:"start_line"`
	EndLine   int `json:"end_line"`

	// Content
	OldContent string `json:"old_content,omitempty"` // verification content
	NewContent string `json:"new_content"`           // replacement or new content

	// Type — one of:
	// "replace_symbol", "add_struct_field", "remove_struct_field",
	// "range_replace", "create_file", "create_or_patch", "rename_file"
	MutationType string `json:"mutation_type"`

	// For vue_inject_directive
	Tag string `json:"tag,omitempty"`
	MatchAttr string `json:"match_attr,omitempty"`
	Directive string `json:"directive,omitempty"`
}

// BatchEditOptions controls batch edit behaviour.
type BatchEditOptions struct {
	DryRun                  bool          `json:"dry_run,omitempty"`
	Atomic                  bool          `json:"atomic,omitempty"`
	StopOnFirstGroupFailure bool          `json:"stop_on_first_group_failure,omitempty"`
	ReadBefore              bool          `json:"read_before,omitempty"`
	TraceBefore             bool          `json:"trace_before,omitempty"`
	CheckEffects            bool          `json:"check_effects,omitempty"`
	ValidateAfter           bool          `json:"validate_after,omitempty"`
	RecordEdit              bool          `json:"record_edit,omitempty"`
	CleanupImports          bool          `json:"cleanup_imports,omitempty"`
	ValidateWiring          bool          `json:"validate_wiring,omitempty"`
	Timeout                 time.Duration `json:"timeout,omitempty"` // per-file lock timeout
}

// GroupResult holds the outcome of a single group.
type GroupResult struct {
	Group              string   `json:"group"`
	Applied            bool     `json:"applied"`
	ChangedFiles       []string `json:"changed_files,omitempty"`
	EffectChanges      []string `json:"effect_changes,omitempty"`
	ValidateViolations []string `json:"validate_violations,omitempty"`
	Error              string   `json:"error,omitempty"`
}

// BatchEditResult is the top-level result from a batch edit.
type BatchEditResult struct {
	Groups         []GroupResult `json:"groups"`
	OverallApplied bool          `json:"overall_applied"`
	OverallDiff    string        `json:"overall_diff,omitempty"`
}

// BatchEdit applies a list of patches organised into groups.
// Groups are processed sequentially. Within a group, patches are coalesced per file
// using the three-bucket strategy, then applied with per-patch immediate reindex.
// File-level locking via flock ensures exclusive access during mutation.
func (s *MCPServer) BatchEdit(patches []BatchPatch, opts BatchEditOptions) (*BatchEditResult, error) {
	if len(patches) == 0 {
		return nil, fmt.Errorf("no patches provided")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}

	// 1. Group patches by group name (default "_default")
	groups := make(map[string][]BatchPatch)
	groupOrder := make([]string, 0)
	for _, p := range patches {
		gname := p.Group
		if gname == "" {
			gname = "_default"
		}
		if _, exists := groups[gname]; !exists {
			groupOrder = append(groupOrder, gname)
		}
		groups[gname] = append(groups[gname], p)
	}

	var results []GroupResult
	var snapshotsAll map[string][]byte // only used if Atomic

	// Helper: rollback all snapshots
	rollbackAll := func() {
		for file, content := range snapshotsAll {
			abs, err := s.idx.Policy.ResolveWorkspacePath(file)
			if err == nil {
				_ = atomicWriteFile(abs, content)
			}
		}
	}

	// Process groups in order
	for _, gname := range groupOrder {
		grp := groups[gname]

		// Coalesce patches per file within the group
		coalesced, err := coalesceFilePatches(grp)
		if err != nil {
			if opts.Atomic {
				rollbackAll()
				return nil, fmt.Errorf("group %s coalesce: %w", gname, err)
			}
			results = append(results, GroupResult{
				Group:   gname,
				Applied: false,
				Error:   err.Error(),
			})
			if opts.StopOnFirstGroupFailure {
				break
			}
			continue
		}

		// Apply the group
		grpResult, snapshots, err := s.applyGroup(gname, coalesced, opts)
		if err != nil && opts.Atomic {
			rollbackAll()
			return nil, fmt.Errorf("group %s apply: %w", gname, err)
		}

		// Accumulate snapshots for atomic rollback
		if opts.Atomic {
			if snapshotsAll == nil {
				snapshotsAll = make(map[string][]byte)
			}
			for f, c := range snapshots {
				snapshotsAll[f] = c
			}
		}

		results = append(results, grpResult)
		if !grpResult.Applied && opts.StopOnFirstGroupFailure {
			break
		}
	}

	// Determine overall success
	overall := len(results) > 0
	for _, r := range results {
		if !r.Applied {
			overall = false
			break
		}
	}

	return &BatchEditResult{
		Groups:         results,
		OverallApplied: overall,
	}, nil
}

// applyGroup processes a single group: acquires flock locks per file, takes snapshots,
// applies patches sequentially, reindexes after each patch, runs pre/post checks,
// and records the edit. On failure, the group's files are rolled back from snapshots.
func (s *MCPServer) applyGroup(groupName string, patches []BatchPatch, opts BatchEditOptions) (GroupResult, map[string][]byte, error) {
	// Acquire flock locks and take snapshots for all files in this group
	snapshots := make(map[string][]byte)
	locks := make(map[string]*flock.Flock)

	for _, p := range patches {
		if _, ok := snapshots[p.File]; ok {
			continue
		}
		abs, err := s.idx.Policy.ResolveWorkspacePath(p.File)
		if err != nil {
			releaseLocks(locks)
			return GroupResult{}, nil, fmt.Errorf("resolve %s: %w", p.File, err)
		}
		// Create flock for the target file (not a separate .lock file)
		fl := flock.New(abs)
		locked, err := fl.TryLockContext(context.Background(), opts.Timeout)
		if err != nil || !locked {
			releaseLocks(locks)
			return GroupResult{}, nil, fmt.Errorf("cannot lock %s: %v", p.File, err)
		}
		locks[p.File] = fl

		data, err := os.ReadFile(abs)
		if err != nil && !os.IsNotExist(err) {
			releaseLocks(locks)
			return GroupResult{}, nil, fmt.Errorf("read %s: %w", p.File, err)
		}
		snapshots[p.File] = data
	}

	// Ensure flock locks are released on exit
	defer releaseLocks(locks)

	// Dry-run with optional pre-read
	if opts.DryRun {
		result := GroupResult{Group: groupName, Applied: false}
		if opts.ReadBefore {
			pre := make(map[string]string)
			for f, content := range snapshots {
				pre[f] = string(content)
			}
			for f, content := range pre {
				lines := strings.Split(content, "\n")
				if len(lines) > 10 {
					lines = lines[:10]
				}
				Log.Debug("read_before", slog.String("file", f), slog.String("preview", strings.Join(lines, "\n")))
			}
		}
		return result, snapshots, nil
	}

	// Pre-effect capture
	var preEffects map[string]*EffectModality
	if opts.TraceBefore && opts.CheckEffects {
		preEffects = s.captureEffectModalities(patches)
		if len(preEffects) > 0 {
			for sym, mod := range preEffects {
				Log.Debug("trace_before", slog.String("symbol", sym), slog.String("error", mod.Error), slog.String("io", mod.IO))
			}
		}
	} else if opts.CheckEffects {
		preEffects = s.captureEffectModalities(patches)
	}

	changedFiles := make(map[string]bool)

	// Apply patches sequentially, reindex after each
	for _, p := range patches {
		err := s.applyPatch(p, snapshots)
		if err != nil {
			// Rollback this group from snapshots
			for f, content := range snapshots {
				abs, err2 := s.idx.Policy.ResolveWorkspacePath(f)
				if err2 == nil {
					_ = atomicWriteFile(abs, content)
				}
			}
			return GroupResult{
				Group:   groupName,
				Applied: false,
				Error:   err.Error(),
			}, snapshots, nil
		}
		changedFiles[p.File] = true

		// Immediate reindex so later patches see updated symbols
		lang, ok := DetectOraLanguage(p.File)
		if ok {
			_ = s.idx.IndexFile(p.File, lang)
		}
	}

	// Post-effect diff
	var effectChanges []string
	if opts.CheckEffects {
		postEffects := s.captureEffectModalities(patches)
		effectChanges = s.diffEffects(preEffects, postEffects)
	}

	// Validate after (constraint validation)
	var violations []string
	if opts.ValidateAfter {
		violations = s.runValidateOnFiles(changedFiles)
	}

	// Cleanup imports
	var removedImports []string
	if opts.CleanupImports {
		for f := range changedFiles {
			abs, _ := s.idx.Policy.ResolveWorkspacePath(f)
			if strings.HasSuffix(f, ".go") {
				removed, _ := removeUnusedImports(abs)
				if len(removed) > 0 {
					removedImports = append(removedImports, f+": "+strings.Join(removed, ", "))
				}
			}
		}
	}

	// Validate wiring
	var wiringViolations []string
	if opts.ValidateWiring {
		wiringViolations, _ = s.validateWiringConstraints(changedFiles)
	}

	// Record edit
	if opts.RecordEdit && s.store != nil {
		var filesList []string
		for f := range changedFiles {
			filesList = append(filesList, f)
		}
		entry := EditEntry{
			Timestamp:    time.Now().UTC().Format(time.RFC3339),
			Task:         "Batch edit group " + groupName,
			Files:        filesList,
			Symbols:      extractSymbolsFromPatches(patches),
			Verification: "auto-recorded",
		}
		_ = s.store.RecordEdit(entry)
	}

	// Combine violations
	allViolations := violations
	allViolations = append(allViolations, removedImports...)
	allViolations = append(allViolations, wiringViolations...)

	return GroupResult{
		Group:              groupName,
		Applied:            true,
		ChangedFiles:       keysOfMap(changedFiles),
		EffectChanges:      effectChanges,
		ValidateViolations: allViolations,
	}, snapshots, nil
}

// releaseLocks unlocks all flock files.
func releaseLocks(locks map[string]*flock.Flock) {
	for _, fl := range locks {
		_ = fl.Unlock()
	}
}

// applyPatch executes one mutation using existing surgical ops or file system operations.
func (s *MCPServer) applyPatch(p BatchPatch, snapshots map[string][]byte) error {
	abs, err := s.idx.Policy.ResolveWorkspacePath(p.File)
	if err != nil {
		return err
	}

	switch p.MutationType {
	case "replace_symbol":
		if p.SymbolAnchor == "" {
			return fmt.Errorf("replace_symbol requires symbol_anchor")
		}
		_, err := s.ops.ReplaceSymbol(p.File, p.SymbolAnchor, p.NewContent)
		return err

		case "replace_symbol_vue":
		if p.SymbolAnchor == "" {
			return fmt.Errorf("replace_symbol_vue requires symbol_anchor")
		}
		fops := NewFrontendOps(s.idx)
		return fops.ReplaceSymbolInVue(p.File, p.SymbolAnchor, p.NewContent)

	case "add_import_vue":
		fops := NewFrontendOps(s.idx)
		return fops.AddImportToVue(p.File, p.NewContent)

	case "add_composable_vue":
		fops := NewFrontendOps(s.idx)
		return fops.AddComposableToSetup(p.File, p.NewContent)

	case "vue_inject_directive":
		fops := NewFrontendOps(s.idx)
		_, err := fops.InjectVueDirective(p.File, p.Tag, p.MatchAttr, p.Directive, false)
		return err

	case "replace_block":
		if p.Block == "" {
			return fmt.Errorf("replace_block requires block")
		}
		fops := NewFrontendOps(s.idx)
		return fops.ReplaceBlock(p.File, p.Block, p.NewContent)

	case "add_struct_field":
		if p.SymbolAnchor == "" {
			return fmt.Errorf("add_struct_field requires symbol_anchor")
		}
		_, err := s.ops.AddStructField(p.File, p.SymbolAnchor, p.NewContent)
		return err

	case "remove_struct_field":
		if p.SymbolAnchor == "" {
			return fmt.Errorf("remove_struct_field requires symbol_anchor")
		}
		_, err := s.ops.RemoveStructField(p.File, p.SymbolAnchor, p.NewContent)
		return err

	case "range_replace":
		if p.StartLine == 0 && p.EndLine == 0 && p.Block == "" {
			return fmt.Errorf("range_replace requires start_line and end_line")
		}

		startLine := p.StartLine
		endLine := p.EndLine

		if p.Block != "" {
			absStart, absEnd, err := ResolveBlockOffset(abs, p.Block, p.StartLine, p.EndLine)
			if err != nil {
				return err
			}
			startLine = absStart
			endLine = absEnd
		}

		data, err := os.ReadFile(abs)
		if err != nil {
			return err
		}
		lines := strings.Split(string(data), "\n")
		if startLine < 1 {
			startLine = 1
		}
		if endLine > len(lines) {
			endLine = len(lines)
		}
		if startLine > endLine {
			return fmt.Errorf("start_line > end_line")
		}

		// Verify old content if provided
		if p.OldContent != "" {
			oldBlock := strings.Join(lines[startLine-1:endLine], "\n")
			if oldBlock != p.OldContent {
				return fmt.Errorf("old_content mismatch in %s at lines %d-%d", p.File, startLine, endLine)
			}
		}

		// Build new lines with splice
		newLines := make([]string, 0, len(lines)-(endLine-startLine+1)+strings.Count(p.NewContent, "\n")+1)
		newLines = append(newLines, lines[:startLine-1]...)
		newLines = append(newLines, strings.Split(p.NewContent, "\n")...)
		if endLine < len(lines) {
			newLines = append(newLines, lines[endLine:]...)
		}
		return atomicWriteFile(abs, []byte(strings.Join(newLines, "\n")))

	case "create_file":
		if _, err := os.Stat(abs); err == nil {
			return fmt.Errorf("file already exists: %s", p.File)
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
			return err
		}
		return atomicWriteFile(abs, []byte(p.NewContent))

	case "create_or_patch":
		if _, err := os.Stat(abs); os.IsNotExist(err) {
			if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
				return err
			}
			return atomicWriteFile(abs, []byte(p.NewContent))
		}
		// File exists — treat as range_replace on the whole file
		if p.OldContent == "" {
			return fmt.Errorf("create_or_patch on existing file requires old_content for verification")
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return err
		}
		if string(data) != p.OldContent {
			return fmt.Errorf("old_content mismatch in existing file %s", p.File)
		}
		return atomicWriteFile(abs, []byte(p.NewContent))

	case "rename_file":
		if p.NewContent == "" {
			return fmt.Errorf("rename_file requires new_content as target path")
		}
		newAbs, err := s.idx.Policy.ResolveWorkspacePath(p.NewContent)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(newAbs), 0755); err != nil {
			return err
		}
		if err := os.Rename(abs, newAbs); err != nil {
			return err
		}
		// Reindex old path (removes it) and new path (adds it)
		lang, _ := DetectOraLanguage(p.File)
		_ = s.idx.IndexFile(p.File, lang)
		lang2, _ := DetectOraLanguage(p.NewContent)
		_ = s.idx.IndexFile(p.NewContent, lang2)
		return nil

	default:
		return fmt.Errorf("unknown mutation_type: %q", p.MutationType)
	}
}

// extractSymbolsFromPatches collects all symbol_anchor values from patches.
func extractSymbolsFromPatches(patches []BatchPatch) []string {
	seen := make(map[string]bool)
	for _, p := range patches {
		if p.SymbolAnchor != "" {
			seen[p.SymbolAnchor] = true
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// keysOfMap returns sorted string keys of a map.
func keysOfMap(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// getBool is a helper to extract a boolean from a JSON-like map.
func getBool(args map[string]interface{}, key string) bool {
	if v, ok := args[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// Ensure sync is imported (used for flock locks).
var _ = sync.Mutex{}

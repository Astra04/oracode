package oracode

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofrs/flock"
	"gopkg.in/yaml.v3"
)

const DefaultIndexWorkers = 6

// astSaveMu prevents Windows "File in Use" errors during concurrent renames
var astSaveMu sync.Mutex

type Location struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Context string `json:"context,omitempty"`
}

type Symbol struct {
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	File       string     `json:"file"`
	Line       int        `json:"line"`
	Column     int        `json:"column"`
	Language   Language   `json:"language"`
	References []Location `json:"references,omitempty"`
	Imports    []string   `json:"imports,omitempty"`
}

type FileMeta struct {
	Path     string   `json:"path"`
	Language Language `json:"language"`
	Checksum string   `json:"checksum,omitempty"`
}

type Index struct {
	mu               sync.RWMutex
	Symbols          map[string][]*Symbol
	FileSymbols      map[string][]*Symbol
	Files            map[string]*FileMeta
	Pool             *ParserPool
	Policy           SecurityPolicy
	Workers          int
	EffectGraph      map[string]*EffectModality `json:"-"` // symbol name -> modality (optional)
	effectGraph      *EffectGraph
	effectGraphMu    sync.RWMutex
	effectGraphDirty bool

	// IndexingActive is true while WalkDirectory is running in the background.
	// Tool calls that check this flag should treat any served data as stale.
	IndexingActive atomic.Bool
}

func NewIndex(policy SecurityPolicy) (*Index, error) {
	_ = loadWorkspaceEnv(policy.WorkspaceRoot)
	pool, err := NewParserPool()
	if err != nil {
		return nil, err
	}
	idx := &Index{
		Symbols:          make(map[string][]*Symbol),
		FileSymbols:      make(map[string][]*Symbol),
		Files:            make(map[string]*FileMeta),
		Pool:             pool,
		Policy:           policy,
		Workers:          DefaultIndexWorkers,
		EffectGraph:      make(map[string]*EffectModality),
		effectGraphDirty: true,
	}
	// Instantly hydrate from the persisted GOB snapshot (if one exists).
	// This makes the first tool call available in <5ms even on a large codebase.
	if err := idx.loadGob(); err == nil {
		fmt.Fprintf(os.Stderr, "[oracode] AST index loaded from cache: %d files, %d symbols\n",
			len(idx.Files), len(idx.Symbols))
	}
	return idx, nil
}

// gobSnapshot is the serialised form of the AST index stored in ast_index.gob.
type gobSnapshot struct {
	Symbols     map[string][]*Symbol
	FileSymbols map[string][]*Symbol
	Files       map[string]*FileMeta
}

// gobPath returns the absolute path to the AST index cache file.
func (idx *Index) gobPath() string {
	return filepath.Join(idx.Policy.WorkspaceRoot, ".oracode", "state", "ast_index.gob")
}

// saveGob serialises the in-memory AST index to ast_index.gob using a temp-file
// + atomic rename pattern to prevent partial writes.
func (idx *Index) saveGob() error {
	// 1. Serialize disk I/O to prevent Windows lock conflicts
	astSaveMu.Lock()
	defer astSaveMu.Unlock()

	p := idx.gobPath()

	// Guarantee directory exists before trying to write/lock
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return fmt.Errorf("mkdir error: %w", err)
	}

	// Use cross-process advisory lock to prevent Windows Lock Violations
	lockPath := p + ".lock"
	fileLock := flock.NewFlock(lockPath)
	if err := fileLock.Lock(); err != nil {
		return fmt.Errorf("failed to acquire AST index lock on %s: %w", lockPath, err)
	}
	defer fileLock.Unlock()

	// 2. Hold RLock across the ENTIRE encode process to prevent map mutation panics
	idx.mu.RLock()
	snap := gobSnapshot{
		Symbols:     idx.Symbols,
		FileSymbols: idx.FileSymbols,
		Files:       idx.Files,
	}
	idx.mu.RUnlock()

	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(snap); err != nil {
		return fmt.Errorf("gob encode error: %w", err)
	}

	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write tmp error: %w", err)
	}

	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename error: %w", err)
	}
	return nil
}

// loadGob hydrates the in-memory AST index from ast_index.gob.
func (idx *Index) loadGob() error {
	p := idx.gobPath()

	// Guarantee directory exists
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}

	lockPath := p + ".lock"
	fileLock := flock.NewFlock(lockPath)
	if err := fileLock.RLock(); err != nil {
		return err
	}
	defer fileLock.Unlock()

	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var snap gobSnapshot
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&snap); err != nil {
		return err
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()
	if snap.Symbols != nil {
		idx.Symbols = snap.Symbols
	}
	if snap.FileSymbols != nil {
		idx.FileSymbols = snap.FileSymbols
	}
	if snap.Files != nil {
		idx.Files = snap.Files
	}
	return nil
}

func (idx *Index) WalkDirectory() error {
	// FLAG ON: Prevent I/O storm by halting saveGob calls during bulk walk
	idx.IndexingActive.Store(true)
	defer idx.IndexingActive.Store(false)

	root := idx.Policy.WorkspaceRoot
	workers := idx.Workers
	if workers <= 0 {
		workers = DefaultIndexWorkers
	}
	if max := runtime.NumCPU(); workers > max {
		workers = max
	}
	if workers < 1 {
		workers = 1
	}

	type job struct {
		rel  string
		lang Language
	}
	jobs := make(chan job, workers*4)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				func(job job) {
					defer func() {
						if r := recover(); r != nil {
							fmt.Fprintf(os.Stderr, "[oracode] panic indexing %s: %v\n", job.rel, r)
						}
					}()
					if err := idx.IndexFile(job.rel, job.lang); err != nil {
						fmt.Fprintf(os.Stderr, "[oracode] indexer error %s: %v\n", job.rel, err)
					}
				}(j)
			}
		}()
	}

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if idx.Policy.ShouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		lang, ok := DetectOraLanguage(path)
		if !ok {
			return nil
		}
		jobs <- job{rel: rel, lang: lang}
		return nil
	})
	close(jobs)
	wg.Wait()

	idx.mu.RLock()
	fmt.Fprintf(os.Stderr, "[oracode] index complete: %d files, %d symbols\n", len(idx.Files), len(idx.Symbols))
	idx.mu.RUnlock()

	if walkErr != nil {
		return walkErr
	}
	if err := idx.RefreshRouteGraph(); err != nil {
		fmt.Fprintf(os.Stderr, "[oracode] route-graph error: %v\n", err)
	}
	if err := idx.BuildAllGraphs(); err != nil {
		fmt.Fprintf(os.Stderr, "[oracode] graph build error: %v\n", err)
	}
	if err := GenerateModuleMap(idx.Policy.WorkspaceRoot); err != nil {
		fmt.Fprintf(os.Stderr, "[oracode] module map generation: %v\n", err)
	}
	// Persist the updated AST index so the next server start loads instantly.
	if err := idx.saveGob(); err != nil {
		fmt.Fprintf(os.Stderr, "[oracode] AST index save error: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "[oracode] AST index saved to cache\n")
	}
	return nil
}

func (idx *Index) IndexFile(relPath string, lang Language) error {
	cst, err := idx.Pool.ParseFile(relPath, lang, idx.Policy)
	if err != nil {
		return fmt.Errorf("failed to parse %s: %w", relPath, err)
	}
	defer cst.Release()

	symbols, err := ExtractSymbols(cst)
	if err != nil {
		return fmt.Errorf("failed to extract symbols from %s: %w", relPath, err)
	}

	idx.mu.Lock()

	idx.Files[relPath] = &FileMeta{
		Path:     relPath,
		Language: lang,
	}

	// Remove previous definitions for this file
	oldSymbols := idx.FileSymbols[relPath]
	for _, oldSym := range oldSymbols {
		idx.removeSymbolByName(oldSym.Name, relPath)
	}

	// Add new definitions
	idx.FileSymbols[relPath] = symbols
	for _, sym := range symbols {
		idx.Symbols[sym.Name] = append(idx.Symbols[sym.Name], sym)
	}

	// Optional: compute effect modality for Go functions (if needed on the fly)
	if lang == LanguageGo {
		idx.MarkEffectGraphDirty()
		for _, sym := range symbols {
			if sym.Kind == "definition" {
				// Re-parse to get effect modality (could be cached, but for simplicity we compute now)
				cst2, err2 := idx.Pool.ParseFile(relPath, lang, idx.Policy)
				if err2 == nil {
					mod := extractModalitiesFromCST(cst2, sym.Name)
					if mod != nil {
						idx.EffectGraph[sym.Name] = mod
					}
					cst2.Release()
				}
			}
		}
	}

	idx.mu.Unlock()

	// Only trigger incremental saves if we are not in the middle of a bulk walk
	if !idx.IndexingActive.Load() {
		if err := idx.saveGob(); err != nil {
			fmt.Fprintf(os.Stderr, "[oracode] index file: saveGob error: %v\n", err)
		}
	}

	return nil
}

func (idx *Index) removeSymbolByName(name, relPath string) {
	syms := idx.Symbols[name]
	var filtered []*Symbol
	for _, s := range syms {
		if s.File != relPath {
			filtered = append(filtered, s)
		}
	}
	if len(filtered) == 0 {
		delete(idx.Symbols, name)
	} else {
		idx.Symbols[name] = filtered
	}
}

// FindDefinitions looks up a symbol by name. Returns a copy of the pointers.
func (idx *Index) FindDefinitions(name string) []*Symbol {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	syms := idx.Symbols[name]
	out := make([]*Symbol, len(syms))
	copy(out, syms)
	return out
}

// FindReferences returns all reference symbols for a given name.
func (idx *Index) FindReferences(name string) []*Symbol {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	var out []*Symbol
	for _, syms := range idx.FileSymbols {
		for _, sym := range syms {
			if sym == nil {
				continue
			}
			if sym.Kind == "reference" && sym.Name == name {
				out = append(out, sym)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File == out[j].File {
			return out[i].Line < out[j].Line
		}
		return out[i].File < out[j].File
	})
	return out
}

// GetEffectModality returns the effect modality for a symbol, if computed.
func (idx *Index) GetEffectModality(name string) *EffectModality {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if idx.EffectGraph == nil {
		return nil
	}
	return idx.EffectGraph[name]
}

func (idx *Index) GetEffectGraph() (*EffectGraph, error) {
	idx.effectGraphMu.RLock()
	if idx.effectGraph != nil && !idx.effectGraphDirty {
		defer idx.effectGraphMu.RUnlock()
		return idx.effectGraph, nil
	}
	idx.effectGraphMu.RUnlock()

	idx.effectGraphMu.Lock()
	defer idx.effectGraphMu.Unlock()
	if idx.effectGraph != nil && !idx.effectGraphDirty {
		return idx.effectGraph, nil
	}
	path := workspaceStatePath(idx.Policy.WorkspaceRoot, "effect_graph.json")
	g, err := LoadEffectGraph(path)
	if err != nil {
		return nil, err
	}
	idx.effectGraph = g
	idx.effectGraphDirty = false
	return idx.effectGraph, nil
}

func (idx *Index) MarkEffectGraphDirty() {
	idx.effectGraphMu.Lock()
	defer idx.effectGraphMu.Unlock()
	idx.effectGraphDirty = true
}

func (idx *Index) buildEffectGraph() error {
	graph := &EffectGraph{
		Symbols: make(map[string]*EffectModality),
	}

	// Increment version from existing graph
	oldPath := workspaceStatePath(idx.Policy.WorkspaceRoot, "effect_graph.json")
	if old, err := LoadEffectGraph(oldPath); err == nil && old != nil {
		graph.Version = old.Version + 1
	} else {
		graph.Version = 1
	}
	graph.Generated = time.Now()

	idx.mu.RLock()
	for rel, meta := range idx.Files {
		if meta.Language != LanguageGo {
			continue
		}
		cst, err := idx.Pool.ParseFile(rel, LanguageGo, idx.Policy)
		if err != nil {
			continue
		}
		syms, _ := ExtractSymbols(cst)
		for _, sym := range syms {
			if sym.Kind == "definition" {
				mod := extractModalitiesFromCST(cst, sym.Name)
				if mod != nil {
					graph.Symbols[sym.Name] = mod
				}
			}
		}
		cst.Release()
	}
	idx.mu.RUnlock()

	path := workspaceStatePath(idx.Policy.WorkspaceRoot, "effect_graph.json")
	// Make sure the parent directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	if err := graph.Save(path); err != nil {
		return err
	}

	idx.effectGraphMu.Lock()
	idx.effectGraph = graph
	idx.effectGraphDirty = false
	idx.effectGraphMu.Unlock()
	return nil
}

func (idx *Index) BuildAllGraphs() error {
	// Build effect graph first
	if err := idx.buildEffectGraph(); err != nil {
		return err
	}
	// Load module map from state cache or workspace seed.
	moduleMap := make(map[string][]string)

	// Try loading from .oracode/module_map.yaml dynamically
	mapPath := filepath.Join(idx.Policy.WorkspaceRoot, ".oracode", "module_map.yaml")
	if data, err := os.ReadFile(mapPath); err == nil {
		var raw struct {
			Modules map[string]struct {
				Backend  interface{} `yaml:"backend"`
				Frontend interface{} `yaml:"frontend"`
			} `yaml:"modules"`
		}
		if err := yaml.Unmarshal(data, &raw); err == nil {
			for name, m := range raw.Modules {
				prefixes := []string{name}
				addPrefixes := func(val interface{}) {
					if val == nil {
						return
					}
					switch v := val.(type) {
					case string:
						if v != "" {
							prefixes = append(prefixes, v)
						}
					case []interface{}:
						for _, item := range v {
							if s, ok := item.(string); ok && s != "" {
								prefixes = append(prefixes, s)
							}
						}
					}
				}
				addPrefixes(m.Backend)
				addPrefixes(m.Frontend)
				moduleMap[name] = uniqueStrings(prefixes)
			}
		}
	}

	// Always ensure corrected default keys exist in moduleMap as fallbacks
	if _, ok := moduleMap["purchasing"]; !ok {
		moduleMap["purchasing"] = []string{"purchasing"}
	}
	if _, ok := moduleMap["inventory"]; !ok {
		moduleMap["inventory"] = []string{"inventory"}
	}
	if _, ok := moduleMap["sales"]; !ok {
		moduleMap["sales"] = []string{"sales"}
	}
	if _, ok := moduleMap["finance"]; !ok {
		moduleMap["finance"] = []string{"finance"}
	}
	if _, ok := moduleMap["audit"]; !ok {
		moduleMap["audit"] = []string{"audit"}
	}

	// Build SQL graph
	sqlGraph, err := BuildSQLGraph(idx.Policy.WorkspaceRoot, moduleMap)
	if err == nil && sqlGraph != nil {
		sqlPath := workspaceStatePath(idx.Policy.WorkspaceRoot, "sql_graph.json")
		sqlGraph.Save(sqlPath)
	}

	// Build Proto graph
	protoGraph, err := BuildProtoGraph(idx.Policy.WorkspaceRoot)
	if err == nil && protoGraph != nil {
		protoPath := workspaceStatePath(idx.Policy.WorkspaceRoot, "proto_graph.json")
		protoGraph.Save(protoPath)
	}

	// Build Vue surface
	vueSurface, err := BuildVueSurface(idx.Policy.WorkspaceRoot, moduleMap)
	if err == nil && vueSurface != nil {
		vuePath := workspaceStatePath(idx.Policy.WorkspaceRoot, "vue_surface.json")
		vueSurface.Save(vuePath)
	}

	// Build frontend API index
	apiIndex, err := BuildFrontendAPIIndex(idx.Policy.WorkspaceRoot)
	if err == nil && apiIndex != nil {
		apiPath := workspaceStatePath(idx.Policy.WorkspaceRoot, "frontend_api.json")
		apiIndex.Save(apiPath)
	}

	return nil
}

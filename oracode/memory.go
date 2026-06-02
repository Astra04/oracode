package oracode

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	oracodeDirName     = ".oracode"
	editLogFileName    = "edit_log.jsonl"
	decisionsFileName  = "decisions.md"
	moduleMapFileName  = "module_map.yaml"
	workflowsFileName  = "workflows.yaml"
	defaultModuleLabel = "unknown"
)

type EditEntry struct {
	Timestamp    string   `json:"timestamp"`
	Task         string   `json:"task"`
	Reason       string   `json:"reason,omitempty"`
	Files        []string `json:"files,omitempty"`
	Symbols      []string `json:"symbols,omitempty"`
	Verification string   `json:"verification,omitempty"`
	Decision     string   `json:"decision,omitempty"`
}

type ModuleInfo struct {
	Name     string
	Owns     []string
	Frontend []string
	Backend  []string
}

type EditStore struct {
	mu               sync.RWMutex
	workspaceRoot    string
	stateRoot        string
	oracodeDir       string
	editLogPath      string
	decisionsPath    string
	moduleMapPath    string
	workflowsPath    string
	modules          []ModuleInfo
	lastModuleMapMts time.Time
}

func NewEditStore(workspaceRoot string) (*EditStore, error) {
	root := filepath.Clean(workspaceRoot)
	oracodeDir := filepath.Join(root, ".oracode")
	stateRoot := workspaceStateRoot(root)
	store := &EditStore{
		workspaceRoot: root,
		stateRoot:     stateRoot,
		oracodeDir:    oracodeDir,
		editLogPath:   filepath.Join(oracodeDir, editLogFileName),
		decisionsPath: filepath.Join(oracodeDir, decisionsFileName),
		moduleMapPath: filepath.Join(oracodeDir, moduleMapFileName),
		workflowsPath: filepath.Join(oracodeDir, workflowsFileName),
	}
	if err := store.ensureScaffold(); err != nil {
		return nil, err
	}
	if err := store.refreshModuleMapIfNeeded(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *EditStore) ensureScaffold() error {
	if err := os.MkdirAll(s.oracodeDir, 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	// Ensure files exist with lightweight templates.
	if err := ensureFile(s.editLogPath, ""); err != nil {
		return err
	}
	if err := ensureFile(s.decisionsPath, "# Jamiiloop Decisions\n\n"); err != nil {
		return err
	}
	if err := ensureFile(s.workflowsPath, "# Workflow notes\n\n"); err != nil {
		return err
	}
	moduleMapTemplate := `# Module ownership map
# Example:
# crm:
#   owns:
#     - customers
#   frontend:
#     - frontend/src/views/crm
#   backend:
#     - api/internal/crm
`
	if err := ensureStateSeed(s.workspaceRoot, s.moduleMapPath, moduleMapFileName); err != nil {
		return err
	}
	if err := ensureFile(s.moduleMapPath, moduleMapTemplate); err != nil {
		return err
	}
	return nil
}

func ensureFile(path string, defaultContent string) error {
	_, err := os.Stat(path)
	if err == nil {
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(defaultContent), 0o644); err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	return nil
}

func (s *EditStore) RecordEdit(entry EditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	if entry.Task == "" {
		return fmt.Errorf("task is required")
	}

	payload, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal edit entry: %w", err)
	}
	f, err := os.OpenFile(s.editLogPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("open edit log: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(string(payload) + "\n"); err != nil {
		return fmt.Errorf("append edit log: %w", err)
	}
	return nil
}

func (s *EditStore) RecentEdits(limit int) ([]EditEntry, error) {
	entries, err := s.readAllEdits()
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > len(entries) {
		limit = len(entries)
	}
	if limit == 0 {
		return []EditEntry{}, nil
	}
	return entries[len(entries)-limit:], nil
}

func (s *EditStore) EditsForFile(file string, limit int) ([]EditEntry, error) {
	entries, err := s.readAllEdits()
	if err != nil {
		return nil, err
	}
	norm := filepath.ToSlash(filepath.Clean(file))
	var out []EditEntry
	for _, e := range entries {
		for _, f := range e.Files {
			if filepath.ToSlash(filepath.Clean(f)) == norm {
				out = append(out, e)
				break
			}
		}
	}
	return tailEntries(out, limit), nil
}

func (s *EditStore) EditsForSymbol(symbol string, limit int) ([]EditEntry, error) {
	entries, err := s.readAllEdits()
	if err != nil {
		return nil, err
	}
	target := strings.TrimSpace(strings.ToLower(symbol))
	var out []EditEntry
	for _, e := range entries {
		for _, sym := range e.Symbols {
			if strings.ToLower(strings.TrimSpace(sym)) == target {
				out = append(out, e)
				break
			}
		}
	}
	return tailEntries(out, limit), nil
}

func tailEntries(entries []EditEntry, limit int) []EditEntry {
	if limit <= 0 || limit > len(entries) {
		limit = len(entries)
	}
	if limit == 0 {
		return []EditEntry{}
	}
	return entries[len(entries)-limit:]
}

func (s *EditStore) readAllEdits() ([]EditEntry, error) {
	s.mu.RLock()
	path := s.editLogPath
	s.mu.RUnlock()

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open edit log: %w", err)
	}
	defer f.Close()

	var entries []EditEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e EditEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan edit log: %w", err)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Timestamp < entries[j].Timestamp
	})
	return entries, nil
}

func (s *EditStore) Decisions(limit int) (string, error) {
	s.mu.RLock()
	path := s.decisionsPath
	s.mu.RUnlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read decisions file: %w", err)
	}
	if limit <= 0 {
		return strings.TrimSpace(string(data)), nil
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if limit >= len(lines) {
		return strings.TrimSpace(string(data)), nil
	}
	return strings.TrimSpace(strings.Join(lines[len(lines)-limit:], "\n")), nil
}

func (s *EditStore) ModuleOwnerForPath(path string) (ModuleInfo, bool, error) {
	if err := s.refreshModuleMapIfNeeded(); err != nil {
		return ModuleInfo{}, false, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	normalized := filepath.ToSlash(filepath.Clean(path))
	best := ModuleInfo{}
	bestScore := -1
	for _, m := range s.modules {
		score := moduleScore(m, normalized)
		if score > bestScore {
			best = m
			bestScore = score
		}
	}
	if bestScore < 0 {
		return ModuleInfo{Name: defaultModuleLabel}, false, nil
	}
	return best, true, nil
}

func moduleScore(m ModuleInfo, path string) int {
	score := -1
	for _, prefix := range append(append([]string{}, m.Frontend...), m.Backend...) {
		p := filepath.ToSlash(filepath.Clean(strings.TrimSpace(prefix)))
		if p == "." || p == "" {
			continue
		}
		if strings.HasPrefix(path, p) && len(p) > score {
			score = len(p)
		}
	}
	return score
}

func (s *EditStore) refreshModuleMapIfNeeded() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	info, err := os.Stat(s.moduleMapPath)
	if err != nil {
		// Try seeding from the workspace copy once if present.
		if seedErr := ensureStateSeed(s.workspaceRoot, s.moduleMapPath, moduleMapFileName); seedErr != nil {
			return seedErr
		}
		info, err = os.Stat(s.moduleMapPath)
		if err != nil {
			return fmt.Errorf("stat module map: %w", err)
		}
	}
	if !info.ModTime().After(s.lastModuleMapMts) && len(s.modules) > 0 {
		return nil
	}

	data, err := os.ReadFile(s.moduleMapPath)
	if err != nil {
		return fmt.Errorf("read module map: %w", err)
	}
	s.modules = parseModuleMapYAML(string(data))
	s.lastModuleMapMts = info.ModTime()
	return nil
}

func parseModuleMapYAML(text string) []ModuleInfo {
	// Use proper YAML parsing to handle both scalar and list formats
	var raw struct {
		Modules map[string]struct {
			Backend  interface{} `yaml:"backend"`
			Frontend interface{} `yaml:"frontend"`
			Owns     interface{} `yaml:"owns"`
		} `yaml:"modules"`
	}
	if err := yaml.Unmarshal([]byte(text), &raw); err != nil {
		return nil
	}
	var out []ModuleInfo
	for name, m := range raw.Modules {
		info := ModuleInfo{Name: name}
		if m.Backend != nil {
			switch v := m.Backend.(type) {
			case string:
				info.Backend = []string{v}
			case []interface{}:
				for _, item := range v {
					if s, ok := item.(string); ok {
						info.Backend = append(info.Backend, s)
					}
				}
			}
		}
		if m.Frontend != nil {
			switch v := m.Frontend.(type) {
			case string:
				info.Frontend = []string{v}
			case []interface{}:
				for _, item := range v {
					if s, ok := item.(string); ok {
						info.Frontend = append(info.Frontend, s)
					}
				}
			}
		}
		if m.Owns != nil {
			switch v := m.Owns.(type) {
			case string:
				info.Owns = []string{v}
			case []interface{}:
				for _, item := range v {
					if s, ok := item.(string); ok {
						info.Owns = append(info.Owns, s)
					}
				}
			}
		}
		out = append(out, info)
	}
	return out
}

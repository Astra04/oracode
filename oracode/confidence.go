package oracode

import (
	"encoding/json"
	"os"
	"sync"
)

type ConfidenceEntry struct {
	Pattern    string `json:"pattern"`    // e.g., "func (*sql.Tx) -> must:rollback"
	Confidence int    `json:"confidence"` // 0-100
	Samples    int    `json:"samples"`
}

type ConfidenceStore struct {
	mu   sync.RWMutex
	path string
	data map[string]ConfidenceEntry
}

func NewConfidenceStore(workspaceRoot string) (*ConfidenceStore, error) {
	path := workspaceSourcePath(workspaceRoot, "confidence_store.json")
	store := &ConfidenceStore{
		path: path,
		data: make(map[string]ConfidenceEntry),
	}
	// Load existing
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &store.data)
	}
	return store, nil
}

func (cs *ConfidenceStore) Record(pattern string, confidence int) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	entry, ok := cs.data[pattern]
	if !ok {
		entry = ConfidenceEntry{Pattern: pattern, Confidence: confidence, Samples: 1}
	} else {
		// weighted average
		entry.Confidence = (entry.Confidence*entry.Samples + confidence) / (entry.Samples + 1)
		entry.Samples++
	}
	cs.data[pattern] = entry
	// save
	data, _ := json.MarshalIndent(cs.data, "", "  ")
	os.WriteFile(cs.path, data, 0644)
}

func (cs *ConfidenceStore) Get(pattern string) (int, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	e, ok := cs.data[pattern]
	return e.Confidence, ok
}

package oracode

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofrs/flock"
	"github.com/wizenheimer/comet"
)

var hideWindow = func(cmd *exec.Cmd) {}
var killExistingLlamaServers = func() {}

// SemanticDocument stores the rich code context for embedding and retrieval.
type SemanticDocument struct {
	ID        string   `json:"id"` // Format: "file.go:SymbolName"
	Symbol    string   `json:"symbol"`
	File      string   `json:"file"`
	Signature string   `json:"signature"`
	Effects   string   `json:"effects"`
	Content   string   `json:"content"`
	FileHash  string   `json:"file_hash"`
	Callers   []string `json:"callers"` // Persisted for BM25 and context

	// Transient scoring fields for agentic multidimensional reasoning
	VectorScore float32 `json:"vector_score,omitempty"`
	BM25Score   float32 `json:"bm25_score,omitempty"`
	FusedScore  float32 `json:"fused_score,omitempty"`

	// IsStale is true when results were returned from the cached/fallback
	// state while background re-indexing is still warming up.
	IsStale bool `json:"is_stale,omitempty"`
}

// SemanticState is the strongly typed struct for gob serialisation
// of the semantic engine's in-memory state. Named types are required
// by encoding/gob for reliable encode/decode across process restarts.
type SemanticState struct {
	Documents map[string]SemanticDocument
	NextID    uint32
	DocMeta   map[uint32]string
}

type SemanticEngine struct {
	mu            sync.RWMutex
	WorkspaceRoot string
	nextID        *atomic.Uint32    // Maps 1:1 with comet's internal ID counter
	docMeta       map[uint32]string // uint32 comet ID -> SemanticDocument.ID (string)

	Documents map[string]SemanticDocument

	Endpoint string
	Model    string

	dirty bool

	vecIndex comet.VectorIndex
	txtIndex comet.TextIndex
	indexMu  sync.RWMutex // Protects vecIndex and txtIndex

	llamaCmd *exec.Cmd

	// WarmingUp is true while the background goroutine is rebuilding the
	// vector + BM25 indices from persisted Documents. Callers should treat
	// any results returned during this window as potentially stale.
	WarmingUp atomic.Bool
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func NewSemanticEngine(workspaceRoot string) *SemanticEngine {
	endpoint := os.Getenv("ORACODE_EMBEDDING_ENDPOINT")
	if endpoint == "" {
		// Point to llama-server's OpenAI-compatible endpoint on port 11435
		endpoint = "http://localhost:11435/v1/embeddings"
	}
	model := os.Getenv("ORACODE_EMBEDDING_MODEL")
	if model == "" {
		model = "nomic-embed-text"
	}

	var next atomic.Uint32
	next.Store(1)

	engine := &SemanticEngine{
		WorkspaceRoot: workspaceRoot,
		Documents:     make(map[string]SemanticDocument),
		docMeta:       make(map[uint32]string),
		Endpoint:      endpoint,
		Model:         model,
		nextID:        &next,
	}

	// 1. Load from disk before index selection to get exact count
	_ = engine.loadDocuments()

	count := len(engine.Documents)

	// 2. Select index type based on exact known size
	if count < 50_000 {
		idx, err := comet.NewFlatIndex(768, comet.Cosine)
		if err == nil {
			engine.vecIndex = idx
		}
	} else {
		m, efC, efS := comet.DefaultHNSWConfig()
		idx, err := comet.NewHNSWIndex(768, comet.Cosine, m, efC, efS)
		if err == nil {
			engine.vecIndex = idx
		}
	}

	if engine.vecIndex == nil {
		engine.vecIndex, _ = comet.NewFlatIndex(768, comet.Cosine)
	}

	// 3. Create BM25 text index
	engine.txtIndex = comet.NewBM25SearchIndex()

	// 4. Auto-start llama-server on port 11435 for embedding generation.
	//    Only if ORACODE_EMBEDDING_ENDPOINT was not explicitly overridden.
	if os.Getenv("ORACODE_EMBEDDING_ENDPOINT") == "" {
		go func(eng *SemanticEngine) {
			// Binary is at <project>/tools/oracode_vnext.exe, so resolve the
			// llama dir relative to <project>/llama-b9439-bin-win-cpu-x64/
			exeDir := filepath.Dir(os.Args[0])
			projectDir := filepath.Dir(exeDir) // go up from tools/ to project root
			llamaDir := filepath.Join(projectDir, "llama-b9439-bin-win-cpu-x64")
			llamaBin := filepath.Join(llamaDir, "llama-server.exe")
			modelPath := filepath.Join(llamaDir, "coderankembed-q8_0.gguf")
			if _, err := os.Stat(llamaBin); err == nil {
				cmd := exec.Command(llamaBin,
					"--port", "11435",
					"--model", modelPath,
					"--embedding",
				)
				hideWindow(cmd)

				killExistingLlamaServers()

				eng.mu.Lock()
				if eng.llamaCmd == nil {
					eng.llamaCmd = cmd
					eng.mu.Unlock()
					fmt.Fprintf(os.Stderr, "[oracode] starting llama-server on port 11435...\n")
					if err := cmd.Start(); err != nil {
						fmt.Fprintf(os.Stderr, "[oracode] llama-server start failed: %v\n", err)
						return
					}
					fmt.Fprintf(os.Stderr, "[oracode] llama-server running (pid %d)\n", cmd.Process.Pid)
					_ = cmd.Wait()
				} else {
					eng.mu.Unlock()
				}
			} else {
				fmt.Fprintf(os.Stderr, "[oracode] llama-server not found at %s, embeddings disabled\n", llamaBin)
			}
		}(engine)
	}

	// 5. Rebuild memory indices asynchronously so the server starts immediately.
	// Any tool call that arrives before warmup completes will receive stale-flagged
	// results from the already-loaded Documents map (linear fallback in HybridSearch).
	engine.WarmingUp.Store(true)
	go func() {
		engine.rebuildIndices()
		engine.WarmingUp.Store(false)
		fmt.Fprintf(os.Stderr, "[oracode] semantic index warmup complete (%d docs ready)\n", len(engine.Documents))
	}()

	return engine
}

// FormatForEmbedding uses the persisted callers array to enrich the semantic payload.
func FormatForEmbedding(doc SemanticDocument) string {
	callerCtx := "None"
	if len(doc.Callers) > 0 {
		limit := len(doc.Callers)
		if limit > 3 {
			limit = 3
		}
		callerCtx = strings.Join(doc.Callers[:limit], ", ")
	}
	return fmt.Sprintf(
		"Symbol: %s\nSignature: %s\nEffects: %s\nCalledBy: %s\nCode:\n%s",
		doc.Symbol, doc.Signature, doc.Effects, callerCtx, doc.Content,
	)
}

func (s *SemanticEngine) GenerateEmbedding(text string) ([]float32, error) {
	reqBody, _ := json.Marshal(map[string]string{
		"model": s.Model,
		"input": text,
	})
	resp, err := httpClient.Post(s.Endpoint, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("embedding service unreachable: %w", err)
	}
	defer resp.Body.Close()

	var res struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("failed to decode embedding response: %w", err)
	}
	if len(res.Data) == 0 {
		return nil, fmt.Errorf("no embedding data returned from server")
	}
	return res.Data[0].Embedding, nil
}

func (s *SemanticEngine) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.llamaCmd != nil && s.llamaCmd.Process != nil {
		_ = s.llamaCmd.Process.Kill()
		_ = s.llamaCmd.Wait()
		s.llamaCmd = nil
	}
}

func (s *SemanticEngine) Upsert(doc SemanticDocument) error {
	textToEmbed := FormatForEmbedding(doc)
	vec, err := s.GenerateEmbedding(textToEmbed)
	if err != nil {
		return err
	}

	// Allocate a comet-compatible uint32 ID
	cid := s.nextID.Add(1)
	node := comet.NewVectorNodeWithID(cid, vec)

	s.mu.Lock()
	// Track the mapping
	s.Documents[doc.ID] = doc
	s.docMeta[cid] = doc.ID
	s.dirty = true
	s.mu.Unlock()

	s.indexMu.Lock()
	// Add to vector index (comet.VectorIndex takes VectorNode, not pointers)
	_ = s.vecIndex.Add(*node)
	// Add to text index (BM25)
	_ = s.txtIndex.Add(cid, textToEmbed)
	s.indexMu.Unlock()

	return nil
}

// Flush writes to disk using a temp file + atomic rename pattern.
func (s *SemanticEngine) Flush() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock() // Release mu during I/O to avoid blocking other readers/writers!

	stateDir := filepath.Join(s.WorkspaceRoot, ".oracode", "state")
	_ = os.MkdirAll(stateDir, 0o755)

	p := filepath.Join(stateDir, "vectors.gob")
	lockPath := p + ".lock"
	fileLock := flock.NewFlock(lockPath)
	if err := fileLock.Lock(); err != nil {
		return fmt.Errorf("failed to acquire vectors save lock: %w", err)
	}
	defer fileLock.Unlock()

	// Snapshot state under RLock
	s.mu.RLock()
	encodeData := SemanticState{
		Documents: s.Documents,
		NextID:    s.nextID.Load(),
		DocMeta:   s.docMeta,
	}
	s.mu.RUnlock()

	// Write to temp file first, then atomically rename
	tmp := p + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}

	err = gob.NewEncoder(f).Encode(encodeData)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "[oracode] FATAL: vectors.gob encode failed: %v\n", err)
		return err
	}

	// Atomic rename
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}

	s.mu.Lock()
	s.dirty = false
	s.mu.Unlock()
	return nil
}

// loadDocuments isolates file reading, called only during init.
func (s *SemanticEngine) loadDocuments() error {
	stateDir := filepath.Join(s.WorkspaceRoot, ".oracode", "state")
	p := filepath.Join(stateDir, "vectors.gob")
	lockPath := p + ".lock"
	_ = os.MkdirAll(stateDir, 0755)

	fileLock := flock.NewFlock(lockPath)
	if err := fileLock.RLock(); err != nil {
		return err
	}
	defer fileLock.Unlock()

	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()

	// Use the strongly typed registered struct (required by encoding/gob)
	var decodeData SemanticState
	if err := gob.NewDecoder(f).Decode(&decodeData); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.Documents = decodeData.Documents
	s.docMeta = decodeData.DocMeta
	if decodeData.NextID > 0 {
		s.nextID.Store(decodeData.NextID)
	}
	return nil
}

// rebuildIndices runs without holding s.mu to prevent deadlocks with comet's internal locks.
func (s *SemanticEngine) rebuildIndices() {
	s.mu.RLock()
	metaCopy := make(map[uint32]string, len(s.docMeta))
	for k, v := range s.docMeta {
		metaCopy[k] = v
	}
	s.mu.RUnlock()

	// Rebuild vector and text indices from persisted data
	for cid, docID := range metaCopy {
		s.mu.RLock()
		doc, ok := s.Documents[docID]
		s.mu.RUnlock()
		if ok {
			s.indexMu.Lock()
			_ = s.vecIndex.Add(*comet.NewVectorNodeWithID(cid, make([]float32, 768)))
			_ = s.txtIndex.Add(cid, FormatForEmbedding(doc))
			s.indexMu.Unlock()
		}
	}
}

func (s *SemanticEngine) HybridSearch(query string, topK int) ([]SemanticDocument, error) {
	// Snapshot warmup state before the potentially slow embedding call.
	isWarmingUp := s.WarmingUp.Load()

	queryVec, err := s.GenerateEmbedding(query)
	if err != nil {
		return nil, err
	}

	s.indexMu.RLock()
	// Vector search using comet's builder pattern
	vecResults, _ := s.vecIndex.NewSearch().
		WithQuery(queryVec).
		WithK(topK * 2).
		Execute()

	// Text/BM25 search
	txtResults, _ := s.txtIndex.NewSearch().
		WithQuery(query).
		WithK(topK * 2).
		Execute()
	s.indexMu.RUnlock()

	// Capture raw scores for multidimensional context
	vecScoreMap := make(map[uint32]float32)
	bm25ScoreMap := make(map[uint32]float32)

	// Reciprocal Rank Fusion (RRF)
	const k = 60
	scores := make(map[uint32]float32)

	for rank, res := range vecResults {
		vecScoreMap[res.Node.ID()] = res.Score
		scores[res.Node.ID()] += 1.0 / float32(k+rank+1)
	}
	for rank, res := range txtResults {
		bm25ScoreMap[res.Id] = res.Score
		scores[res.Id] += 1.0 / float32(k+rank+1)
	}

	type rrfHit struct {
		cid   uint32
		score float32
	}
	var fused []rrfHit
	for cid, score := range scores {
		fused = append(fused, rrfHit{cid, score})
	}

	sort.Slice(fused, func(i, j int) bool {
		return fused[i].score > fused[j].score
	})

	s.mu.RLock()
	defer s.mu.RUnlock()

	var finalDocs []SemanticDocument
	for i := 0; i < topK && i < len(fused); i++ {
		if docID, ok := s.docMeta[fused[i].cid]; ok {
			if doc, exists := s.Documents[docID]; exists {
				// Inject the exact dimensional scores into the result payload
				doc.FusedScore = fused[i].score
				doc.VectorScore = vecScoreMap[fused[i].cid]
				doc.BM25Score = bm25ScoreMap[fused[i].cid]
				doc.IsStale = isWarmingUp
				finalDocs = append(finalDocs, doc)
			}
		}
	}

	// --- Linear Fallback during warmup ---
	// If the vector/BM25 indices are still being built and we haven't filled
	// the quota, scan the already-loaded Documents map directly. This guarantees
	// the LLM gets context immediately rather than an empty response.
	if isWarmingUp && len(finalDocs) < topK {
		queryLower := strings.ToLower(query)
		terms := strings.Fields(queryLower)

		// Build a set of already-returned IDs for dedup
		seenIDs := make(map[string]bool, len(finalDocs))
		for _, fd := range finalDocs {
			seenIDs[fd.ID] = true
		}

		type fallbackHit struct {
			doc     SemanticDocument
			matches int
		}
		var hits []fallbackHit

		for _, doc := range s.Documents {
			if seenIDs[doc.ID] {
				continue
			}
			docLower := strings.ToLower(doc.Symbol + " " + doc.Signature + " " + doc.Content)
			matches := 0
			for _, term := range terms {
				if strings.Contains(docLower, term) {
					matches++
				}
			}
			if matches > 0 {
				hits = append(hits, fallbackHit{doc: doc, matches: matches})
			}
		}

		// Sort fallback hits by match count descending
		sort.Slice(hits, func(i, j int) bool {
			return hits[i].matches > hits[j].matches
		})

		for _, h := range hits {
			if len(finalDocs) >= topK {
				break
			}
			d := h.doc
			d.IsStale = true
			d.BM25Score = float32(h.matches) // repurposed as keyword match count
			finalDocs = append(finalDocs, d)
		}
	}

	return finalDocs, nil
}

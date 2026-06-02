package oracode

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type Server struct {
	Index *Index
}

func NewServer(idx *Index) *Server {
	return &Server{Index: idx}
}

func (s *Server) Start(port int) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/symbol", s.handleSymbol)
	mux.HandleFunc("/dependencies", s.handleDependencies)
	mux.HandleFunc("/sql/tables", s.handleSQLTables)
	mux.HandleFunc("/reindex", s.handleReindex)

	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("OraCode Scalpel server listening on %s\n", addr)
	return http.ListenAndServe(addr, mux)
}

func (s *Server) handleSymbol(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "missing 'name' parameter", http.StatusBadRequest)
		return
	}

	syms := s.Index.FindDefinitions(name)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"symbol":  name,
		"matches": syms,
	})
}

func (s *Server) handleDependencies(w http.ResponseWriter, r *http.Request) {
	sym := r.URL.Query().Get("symbol")

	// Returns everywhere this symbol is referenced
	s.Index.mu.RLock()
	var refs []*Symbol
	for _, symList := range s.Index.Symbols {
		for _, symbol := range symList {
			if symbol.Name == sym && symbol.Kind == "reference" {
				refs = append(refs, symbol)
			}
		}
	}
	s.Index.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"symbol":     sym,
		"references": refs,
	})
}

func (s *Server) handleSQLTables(w http.ResponseWriter, r *http.Request) {
	s.Index.mu.RLock()
	var tables []*Symbol
	for _, symList := range s.Index.Symbols {
		for _, symbol := range symList {
			if symbol.Language == LanguageSQL && symbol.Kind == "definition" {
				tables = append(tables, symbol)
			}
		}
	}
	s.Index.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"tables": tables,
	})
}

func (s *Server) handleReindex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	file := r.URL.Query().Get("file")
	if file == "" {
		http.Error(w, "missing 'file' parameter", http.StatusBadRequest)
		return
	}

	lang, ok := DetectOraLanguage(file)
	if !ok {
		http.Error(w, "unsupported language", http.StatusBadRequest)
		return
	}

	if err := s.Index.IndexFile(file, lang); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = s.Index.RefreshRouteGraph()

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

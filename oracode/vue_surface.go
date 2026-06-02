package oracode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type VueComponent struct {
	Path             string   `json:"path"`
	Module           string   `json:"module"`
	Composables      []string `json:"composables"`
	ApiCalls         []string `json:"apiCalls"`
	MissingCleanup   []string `json:"missingCleanup"`
	HasErrorBoundary bool     `json:"hasErrorBoundary"`
}

type VueSurface struct {
	Version    int                     `json:"version"`
	Generated  string                  `json:"generated"`
	Components map[string]VueComponent `json:"components"`
}

func BuildVueSurface(workspaceRoot string, moduleMap map[string][]string) (*VueSurface, error) {
	surface := &VueSurface{
		Version:    1,
		Generated:  time.Now().Format(time.RFC3339),
		Components: make(map[string]VueComponent),
	}

	var vueFiles []string
	err := filepath.WalkDir(workspaceRoot, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".vue") {
			vueFiles = append(vueFiles, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Regular expressions for heuristic analysis
	composableRe := regexp.MustCompile(`\buse[A-Z][a-zA-Z0-9]*\b`)
	apiCallRe := regexp.MustCompile(`(fetch|axios|http\.get|http\.post|api\.)\s*\(`)
	onUnmountedRe := regexp.MustCompile(`onUnmounted\s*\(`)
	errorBoundaryRe := regexp.MustCompile(`catch\s*\(|try\s*\{`)

	for _, path := range vueFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		blocks, err := ParseVueSFC(bytes.NewReader(data))
		if err != nil {
			continue
		}
		var scriptContent string
		for _, block := range blocks {
			if block.Tag == "script" {
				scriptContent = block.Content
				break
			}
		}
		if scriptContent == "" {
			continue
		}

		comp := VueComponent{
			Path:           path,
			Composables:    uniqueStrings(composableRe.FindAllString(scriptContent, -1)),
			ApiCalls:       uniqueStrings(apiCallRe.FindAllString(scriptContent, -1)),
			MissingCleanup: []string{},
		}

		// Check for missing cleanup: if there is a composable but no onUnmounted
		if len(comp.Composables) > 0 && !onUnmountedRe.MatchString(scriptContent) {
			comp.MissingCleanup = append(comp.MissingCleanup, "no onUnmounted cleanup")
		}
		comp.HasErrorBoundary = errorBoundaryRe.MatchString(scriptContent)

		// Determine module
		for mod, prefixes := range moduleMap {
			for _, prefix := range prefixes {
				if strings.Contains(path, prefix) {
					comp.Module = mod
					break
				}
			}
		}
		surface.Components[path] = comp
	}
	return surface, nil
}

func (s *VueSurface) Save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func LoadVueSurface(path string) (*VueSurface, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s VueSurface
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

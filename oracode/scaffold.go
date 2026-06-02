package oracode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

// ScaffoldFile describes a single file to create from a template.
type ScaffoldFile struct {
	Path     string            `json:"path"`
	Template string            `json:"template"`
	Vars     map[string]string `json:"vars"`
}

// ScaffoldManifest describes a set of files to scaffold.
type ScaffoldManifest struct {
	Name  string         `json:"name"`
	Files []ScaffoldFile `json:"files"`
}

// Scaffold creates files from a manifest using templates stored in .oracode/templates/.
func (s *MCPServer) Scaffold(manifestPath string, dryRun bool) ([]string, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", manifestPath, err)
	}
	var manifest ScaffoldManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", manifestPath, err)
	}

	templatesDir := filepath.Join(s.idx.Policy.WorkspaceRoot, ".oracode", "templates")
	var created []string

	for _, file := range manifest.Files {
		tmplPath := filepath.Join(templatesDir, file.Template)
		tmplContent, err := os.ReadFile(tmplPath)
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", file.Template, err)
		}

		tmpl, err := template.New(file.Template).Option("missingkey=error").Parse(string(tmplContent))
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", file.Template, err)
		}

		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, file.Vars); err != nil {
			return nil, fmt.Errorf("execute template %s: %w", file.Template, err)
		}

		// Use write-safe resolution — file may not exist yet
		absPath, err := s.idx.Policy.ResolveWorkspacePathForWrite(file.Path)
		if err != nil {
			return nil, fmt.Errorf("resolve path %s: %w", file.Path, err)
		}

		if !dryRun {
			if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
				return nil, fmt.Errorf("mkdir %s: %w", filepath.Dir(absPath), err)
			}
			if err := atomicWriteFile(absPath, buf.Bytes()); err != nil {
				return nil, fmt.Errorf("write %s: %w", file.Path, err)
			}
			// Index the new file
			lang, ok := DetectOraLanguage(file.Path)
			if ok {
				_ = s.idx.IndexFile(file.Path, lang)
			}
		}
		created = append(created, file.Path)
	}
	return created, nil
}

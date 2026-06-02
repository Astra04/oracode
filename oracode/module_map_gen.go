package oracode

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type ModuleBoundary struct {
	Backend   string   `yaml:"backend,omitempty"`
	Frontend  string   `yaml:"frontend,omitempty"`
	Contracts []string `yaml:"contracts,omitempty"`
}

type ModuleMap struct {
	Modules map[string]ModuleBoundary `yaml:"modules"`
}

// GenerateModuleMap dynamically discovers backend and frontend module directories
// using a recursive search to avoid workspace root misalignments.
func GenerateModuleMap(workspaceRoot string) error {
	modMap := ModuleMap{Modules: make(map[string]ModuleBoundary)}

	err := filepath.WalkDir(workspaceRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		// Skip heavy directories to drastically speed up mapping
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git" || d.Name() == ".venv" || d.Name() == ".gocache") {
			return filepath.SkipDir
		}

		if d.IsDir() {
			// Backend Module Discovery
			if d.Name() == "internal" || d.Name() == "pkg" || d.Name() == "cmd" {
				entries, _ := os.ReadDir(path)
				for _, e := range entries {
					if e.IsDir() {
						modName := strings.ToLower(e.Name())
						bound := modMap.Modules[modName]
						relPath, _ := filepath.Rel(workspaceRoot, filepath.Join(path, e.Name()))
						bound.Backend = filepath.ToSlash(relPath)
						modMap.Modules[modName] = bound
					}
				}
			}

			// Frontend Module Discovery
			if d.Name() == "views" || d.Name() == "pages" {
				entries, _ := os.ReadDir(path)
				for _, e := range entries {
					if e.IsDir() {
						modName := strings.ToLower(e.Name())
						bound := modMap.Modules[modName]
						relPath, _ := filepath.Rel(workspaceRoot, filepath.Join(path, e.Name()))
						bound.Frontend = filepath.ToSlash(relPath)
						modMap.Modules[modName] = bound
					}
				}
			}
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("walkdir failed: %w", err)
	}

	if len(modMap.Modules) == 0 {
		return nil
	}

	data, err := yaml.Marshal(&modMap)
	if err != nil {
		return fmt.Errorf("marshal module map: %w", err)
	}

	configDir := filepath.Join(workspaceRoot, ".oracode")
	_ = os.MkdirAll(configDir, 0o755)

	mapPath := filepath.Join(configDir, "module_map.yaml")
	if err := os.WriteFile(mapPath, data, 0o644); err != nil {
		return fmt.Errorf("write module map: %w", err)
	}
	return nil
}

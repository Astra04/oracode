package oracode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type APICall struct {
	Component string `json:"component"`
	Function  string `json:"function"`
	URL       string `json:"url"`
	Method    string `json:"method"`
	Line      int    `json:"line"`
}

type FrontendAPIIndex struct {
	Version   int       `json:"version"`
	Generated string    `json:"generated"`
	Calls     []APICall `json:"calls"`
}

// BuildFrontendAPIIndex scans Vue/JS/TS files for API requests.
func BuildFrontendAPIIndex(workspaceRoot string) (*FrontendAPIIndex, error) {
	index := &FrontendAPIIndex{
		Version:   1,
		Generated: time.Now().Format(time.RFC3339),
		Calls:     []APICall{},
	}

	var frontendFiles []string
	err := filepath.WalkDir(workspaceRoot, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			// FIXED: Now scans Javascript/Typescript service files in addition to Vue files
			if strings.HasSuffix(path, ".vue") || strings.HasSuffix(path, ".js") || strings.HasSuffix(path, ".ts") {
				frontendFiles = append(frontendFiles, path)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// UPDATED: Broadly match custom API instances ($api, api, axios, $http) and support backticks (`)
	apiRe := regexp.MustCompile(`(?i)(?:\$api|api|axios|this\.\$http|this\.\$api)\.(get|post|put|delete|patch)\s*\(\s*['"\x60]([^'"\x60]+)['"\x60]`)
	fetchRe := regexp.MustCompile(`(?i)fetch\s*\(\s*['"\x60]([^'"\x60]+)['"\x60]`)

	for _, path := range frontendFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		content := string(data)

		// For Vue files, scope to script block; for JS/TS, scan entire file
		var scriptContent string
		if strings.HasSuffix(path, ".vue") {
			scriptStart := strings.Index(content, "<script")
			if scriptStart == -1 {
				continue
			}
			scriptEnd := strings.Index(content[scriptStart:], "</script>")
			if scriptEnd == -1 {
				continue
			}
			scriptContent = content[scriptStart : scriptStart+scriptEnd]
		} else {
			scriptContent = content
		}

		funcRe := regexp.MustCompile(`(?:const|function|async\s+function)\s+(\w+)\s*\(`)
		currentFunc := ""
		lines := strings.Split(scriptContent, "\n")

		for i, line := range lines {
			if f := funcRe.FindStringSubmatch(line); len(f) > 1 {
				currentFunc = f[1]
			}

			// Match all custom wrappers
			if matches := apiRe.FindStringSubmatch(line); len(matches) > 2 {
				index.Calls = append(index.Calls, APICall{
					Component: path,
					Function:  currentFunc,
					URL:       matches[2],
					Method:    strings.ToUpper(matches[1]),
					Line:      i + 1,
				})
			}
			// Match raw fetch
			if matches := fetchRe.FindStringSubmatch(line); len(matches) > 1 {
				index.Calls = append(index.Calls, APICall{
					Component: path,
					Function:  currentFunc,
					URL:       matches[1],
					Method:    "GET",
					Line:      i + 1,
				})
			}
		}
	}
	return index, nil
}

func (idx *FrontendAPIIndex) Save(path string) error {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func LoadFrontendAPIIndex(path string) (*FrontendAPIIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var idx FrontendAPIIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	return &idx, nil
}

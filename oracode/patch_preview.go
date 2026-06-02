package oracode

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
)

type PatchPreview struct {
	Diff      string `json:"diff"`
	Applied   bool   `json:"applied"`
	File      string `json:"file"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

type PatchHunk struct {
	File       string `json:"file"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	NewContent string `json:"new_content"`
}

// ApplyPatchPreview generates a diff and optionally applies the patch.
// If apply = false, only returns diff. If apply = true, writes the change and reindexes.
func (s *MCPServer) ApplyPatchPreview(file string, startLine, endLine int, newContent string, apply bool) (*PatchPreview, error) {
	previews, err := s.ApplyPatchPreviewBatch([]PatchHunk{{File: file, StartLine: startLine, EndLine: endLine, NewContent: newContent}}, apply)
	if err != nil {
		return nil, err
	}
	if len(previews) != 1 {
		return nil, fmt.Errorf("unexpected number of previews: %d", len(previews))
	}
	return previews[0], nil
}

func (s *MCPServer) ApplyPatchPreviewBatch(hunks []PatchHunk, apply bool) ([]*PatchPreview, error) {
	if len(hunks) == 0 {
		return nil, fmt.Errorf("no patches provided")
	}

	fileHunks := make(map[string][]PatchHunk)
	for _, h := range hunks {
		if h.File == "" {
			return nil, fmt.Errorf("patch file is required")
		}
		if h.NewContent == "" {
			return nil, fmt.Errorf("patch new_content is required for %s", h.File)
		}
		if h.StartLine < 1 {
			h.StartLine = 1
		}
		if h.EndLine < h.StartLine {
			return nil, fmt.Errorf("patch end_line must be >= start_line for %s", h.File)
		}
		fileHunks[h.File] = append(fileHunks[h.File], h)
	}

	var previews []*PatchPreview
	modifiedFiles := make(map[string][]byte)

	for file, hunks := range fileHunks {
		sort.SliceStable(hunks, func(i, j int) bool {
			if hunks[i].StartLine == hunks[j].StartLine {
				return hunks[i].EndLine < hunks[j].EndLine
			}
			return hunks[i].StartLine < hunks[j].StartLine
		})

		absPath, err := s.idx.Policy.ResolveWorkspacePath(file)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(absPath)
		if err != nil {
			return nil, err
		}
		lines := strings.Split(string(data), "\n")

		lastEnd := 0
		newLines := make([]string, 0, len(lines)+len(hunks)*4)

		for _, h := range hunks {
			if h.StartLine > len(lines) {
				return nil, fmt.Errorf("patch start_line %d beyond end of %s (%d lines)", h.StartLine, file, len(lines))
			}
			if h.EndLine > len(lines) {
				h.EndLine = len(lines)
			}
			if h.StartLine <= lastEnd {
				return nil, fmt.Errorf("patches overlap or are out of order for %s", file)
			}

			oldBlock := strings.Join(lines[h.StartLine-1:h.EndLine], "\n")
			dmp := diffmatchpatch.New()
			diffs := dmp.DiffMain(oldBlock, h.NewContent, false)
			previews = append(previews, &PatchPreview{
				Diff:      dmp.DiffPrettyText(diffs),
				Applied:   false,
				File:      file,
				StartLine: h.StartLine,
				EndLine:   h.EndLine,
			})

			newLines = append(newLines, lines[lastEnd:h.StartLine-1]...)
			newLines = append(newLines, strings.Split(h.NewContent, "\n")...)
			lastEnd = h.EndLine
		}

		if lastEnd < len(lines) {
			newLines = append(newLines, lines[lastEnd:]...)
		}

		modifiedFiles[file] = []byte(strings.Join(newLines, "\n"))
	}

	if apply {
		for file, newContents := range modifiedFiles {
			if err := s.validatePatchedFile(file, newContents); err != nil {
				return nil, err
			}
		}

		for file, newContents := range modifiedFiles {
			absPath, err := s.idx.Policy.ResolveWorkspacePath(file)
			if err != nil {
				return nil, err
			}
			// Defensive: tests or callers may provide a partially-initialized Index.
			if s.idx != nil {
				if s.idx.Symbols == nil {
					s.idx.Symbols = make(map[string][]*Symbol)
				}
				if s.idx.FileSymbols == nil {
					s.idx.FileSymbols = make(map[string][]*Symbol)
				}
				if s.idx.Files == nil {
					s.idx.Files = make(map[string]*FileMeta)
				}
			}
			if err := os.WriteFile(absPath, newContents, 0644); err != nil {
				return nil, err
			}
			lang, _ := DetectOraLanguage(file)
			if err := s.idx.IndexFile(file, lang); err == nil {
				_ = s.idx.RefreshRouteGraph()
			}
		}
		for _, preview := range previews {
			preview.Applied = true
		}
	}

	return previews, nil
}

func (s *MCPServer) validatePatchedFile(file string, content []byte) error {
	lang, ok := DetectOraLanguage(file)
	if !ok {
		return nil
	}

	cst, err := s.idx.Pool.ParseSource(file, lang, content)
	if err != nil {
		return fmt.Errorf("syntax validation failed for %s: %w", file, err)
	}
	defer cst.Release()
	if cst.HasError() {
		return fmt.Errorf("syntax validation failed for %s: parse errors detected", file)
	}
	return nil
}

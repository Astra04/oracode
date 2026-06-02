package oracode

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type PatternMatch struct {
	File     string            `json:"file"`
	Line     int               `json:"line"`
	Captures map[string]string `json:"captures,omitempty"`
	Preview  string            `json:"preview,omitempty"`
}

func (idx *Index) ApplyPattern(lang, pattern, replacement, scope string, dryRun bool) ([]PatternMatch, error) {
	targetLang, err := LanguageForName(lang)
	if err != nil {
		return nil, err
	}
	root := idx.Policy.WorkspaceRoot
	if strings.TrimSpace(scope) != "" {
		abs, err := idx.Policy.ResolveWorkspacePath(scope)
		if err != nil {
			return nil, err
		}
		root = abs
	}
	re, captureNames, err := compileCapturePattern(pattern)
	if err != nil {
		return nil, err
	}

	var files []string
	stat, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !stat.IsDir() {
		if langOK, ok := DetectOraLanguage(root); ok && langOK == targetLang {
			files = append(files, root)
		}
	} else {
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if idx.Policy.ShouldSkipDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			fileLang, ok := DetectOraLanguage(path)
			if !ok || fileLang != targetLang {
				return nil
			}
			files = append(files, path)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	var matches []PatternMatch
	changedFiles := make(map[string]struct{})
	for _, absPath := range files {
		data, err := os.ReadFile(absPath)
		if err != nil {
			continue
		}
		src := string(data)
		spans := re.FindAllStringSubmatchIndex(src, -1)
		if len(spans) == 0 {
			continue
		}
		relPath, err := filepath.Rel(idx.Policy.WorkspaceRoot, absPath)
		if err != nil {
			relPath = absPath
		}
		relPath = filepath.ToSlash(relPath)

		for _, span := range spans {
			captures := make(map[string]string, len(captureNames))
			if len(span) >= 2 {
				start, end := span[0], span[1]
				for i, name := range captureNames {
					groupIndex := (i + 1) * 2
					if groupIndex+1 >= len(span) {
						continue
					}
					if span[groupIndex] >= 0 && span[groupIndex+1] >= 0 && span[groupIndex+1] >= span[groupIndex] {
						captures[name] = src[span[groupIndex]:span[groupIndex+1]]
					}
				}
				matches = append(matches, PatternMatch{
					File:     relPath,
					Line:     1 + strings.Count(src[:start], "\n"),
					Captures: captures,
					Preview:  strings.TrimSpace(src[start:end]),
				})
			}
		}

		if dryRun {
			continue
		}
		rewritten, changed := replacePatternMatches(src, re, captureNames, replacement)
		if !changed {
			continue
		}
		if err := os.WriteFile(absPath, []byte(rewritten), 0o644); err != nil {
			return nil, err
		}
		changedFiles[relPath] = struct{}{}
	}

	if !dryRun && len(changedFiles) > 0 {
		for rel := range changedFiles {
			langDetected, ok := DetectOraLanguage(rel)
			if !ok {
				continue
			}
			if err := idx.IndexFile(rel, langDetected); err != nil {
				return nil, fmt.Errorf("reindex %s: %w", rel, err)
			}
		}
		_ = idx.RefreshRouteGraph()
		_ = idx.buildEffectGraph()
	}

	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].File == matches[j].File {
			return matches[i].Line < matches[j].Line
		}
		return matches[i].File < matches[j].File
	})
	return matches, nil
}

func compileCapturePattern(pattern string) (*regexp.Regexp, []string, error) {
	captureRe := regexp.MustCompile(`\$+([A-Za-z_][A-Za-z0-9_]*)`)
	names := make([]string, 0, 8)
	markerPattern := captureRe.ReplaceAllStringFunc(pattern, func(token string) string {
		m := captureRe.FindStringSubmatch(token)
		if len(m) < 2 {
			return token
		}
		name := m[1]
		names = append(names, name)
		return "\x00CAP_" + name + "\x00"
	})
	escaped := regexp.QuoteMeta(markerPattern)
	for _, name := range names {
		marker := regexp.QuoteMeta("\x00CAP_" + name + "\x00")
		escaped = strings.ReplaceAll(escaped, marker, `(?P<`+name+`>[\s\S]+?)`)
	}
	re, err := regexp.Compile("(?s)" + escaped)
	if err != nil {
		return nil, nil, err
	}
	return re, names, nil
}

func replacePatternMatches(src string, re *regexp.Regexp, captureNames []string, replacement string) (string, bool) {
	indices := re.FindAllStringSubmatchIndex(src, -1)
	if len(indices) == 0 {
		return src, false
	}
	var out strings.Builder
	cursor := 0
	changed := false
	for _, idxs := range indices {
		if len(idxs) < 2 {
			continue
		}
		start, end := idxs[0], idxs[1]
		if start < cursor {
			continue
		}
		out.WriteString(src[cursor:start])
		captures := make(map[string]string, len(captureNames))
		for i, name := range captureNames {
			groupIndex := (i + 1) * 2
			if groupIndex+1 >= len(idxs) {
				continue
			}
			if idxs[groupIndex] >= 0 && idxs[groupIndex+1] >= 0 && idxs[groupIndex+1] >= idxs[groupIndex] {
				captures[name] = src[idxs[groupIndex]:idxs[groupIndex+1]]
			}
		}
		out.WriteString(expandReplacement(replacement, captures))
		cursor = end
		changed = true
	}
	out.WriteString(src[cursor:])
	return out.String(), changed
}

func expandReplacement(replacement string, captures map[string]string) string {
	captureRe := regexp.MustCompile(`\$+([A-Za-z_][A-Za-z0-9_]*)`)
	return captureRe.ReplaceAllStringFunc(replacement, func(token string) string {
		m := captureRe.FindStringSubmatch(token)
		if len(m) < 2 {
			return token
		}
		if val, ok := captures[m[1]]; ok {
			return val
		}
		return ""
	})
}

// ApplyStructuralPattern applies a structural pattern replacement to a file,
// handling Vue SFC block offsets if block is specified.
// If dryRun is true, only returns matches without modifying files.
func (s *MCPServer) ApplyStructuralPattern(file, lang, pattern, replacement, block string, dryRun bool) ([]PatternMatch, error) {
	absPath, err := s.idx.Policy.ResolveWorkspacePath(file)
	if err != nil {
		return nil, err
	}
	src, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}

	var content []byte
	var blockStartOffset int

	// Parse Vue SFC if needed
	if lang == "vue" && block != "" {
		blocks, err := ParseVueSFC(bytes.NewReader(src))
		if err != nil {
			return nil, fmt.Errorf("parse vue sfc: %w", err)
		}
		var target *SFCBlock
		for i := range blocks {
			if blocks[i].Tag == block {
				target = &blocks[i]
				break
			}
		}
		if target == nil {
			return nil, fmt.Errorf("block %q not found in %s", block, file)
		}
		// Compute block content slice
		content = []byte(target.Content)
		blockStartOffset = target.StartOffset + len(target.OpenTagRaw)
	} else {
		content = src
		blockStartOffset = 0
	}

	// Compile pattern
	re, captureNames, err := compileCapturePattern(pattern)
	if err != nil {
		return nil, fmt.Errorf("compile pattern: %w", err)
	}

	// Find matches
	text := string(content)
	spans := re.FindAllStringSubmatchIndex(text, -1)
	if len(spans) == 0 {
		return []PatternMatch{}, nil
	}

	// Build matches list with adjusted line numbers
	var matches []PatternMatch
	linesBefore := strings.Count(string(src[:blockStartOffset]), "\n")
	for _, idxs := range spans {
		if len(idxs) < 2 {
			continue
		}
		start, end := idxs[0], idxs[1]
		captures := make(map[string]string)
		for i, name := range captureNames {
			groupIdx := (i + 1) * 2
			if groupIdx+1 < len(idxs) && idxs[groupIdx] >= 0 {
				captures[name] = text[idxs[groupIdx]:idxs[groupIdx+1]]
			}
		}
		// Compute global line number: lines before block + lines within block
		startLine := linesBefore + 1 + strings.Count(text[:start], "\n")
		matches = append(matches, PatternMatch{
			File:     file,
			Line:     startLine,
			Captures: captures,
			Preview:  strings.TrimSpace(text[start:end]),
		})
	}

	if dryRun {
		return matches, nil
	}

	// Apply replacement to the content
	newContent, changed := replacePatternMatches(text, re, captureNames, replacement)
	if !changed {
		return matches, nil
	}

	// Write back to file with block offset translation
	var finalBytes []byte
	if lang == "vue" && block != "" {
		blocks, err := ParseVueSFC(bytes.NewReader(src))
		if err != nil {
			return nil, err
		}
		for i := range blocks {
			if blocks[i].Tag == block {
				blocks[i].Content = newContent
				break
			}
		}
		finalBytes = ReplaceSFCBlocks(src, blocks)
	} else {
		finalBytes = []byte(newContent)
	}

	if err := atomicWriteFile(absPath, finalBytes); err != nil {
		return nil, fmt.Errorf("write file: %w", err)
	}

	// Reindex
	detectedLang, _ := DetectOraLanguage(file)
	_ = s.idx.IndexFile(file, detectedLang)
	if detectedLang == LanguageGo {
		_ = s.idx.RefreshRouteGraph()
		s.idx.MarkEffectGraphDirty()
	}

	return matches, nil
}

package oracode

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// InjectVueDirective safely injects a directive (like v-if) into a specific Vue component tag.
func (f *FrontendOps) InjectVueDirective(file, tag, matchAttr, directive string, dryRun bool) (SurgicalEditResult, error) {
	absPath, err := f.idx.Policy.ResolveWorkspacePath(file)
	if err != nil {
		return SurgicalEditResult{}, err
	}
	src, err := os.ReadFile(absPath)
	if err != nil {
		return SurgicalEditResult{}, fmt.Errorf("read %s: %w", file, err)
	}

	// Relies on existing ParseVueSFC from sfc_parser.go
	blocks, err := ParseVueSFC(bytes.NewReader(src))
	if err != nil {
		return SurgicalEditResult{}, fmt.Errorf("parse sfc %s: %w", file, err)
	}

	var templateBlockIndex = -1
	for i, b := range blocks {
		if b.Tag == "template" {
			templateBlockIndex = i
			break
		}
	}

	if templateBlockIndex == -1 {
		return SurgicalEditResult{}, fmt.Errorf("no <template> block found in %s", file)
	}

	templateContent := blocks[templateBlockIndex].Content
	changed := false
	injectedCount := 0

	// Quote-aware regex: Matches non-quote/non-> chars, OR double-quoted strings, OR single-quoted strings
	reStr := fmt.Sprintf(`(?is)(<%s\b)((?:[^>'"]*(?:"[^"]*"|'[^']*'))*[^>'"]*)(\/?>)`, regexp.QuoteMeta(tag))
	re := regexp.MustCompile(reStr)

	directiveKey := strings.Split(strings.TrimSpace(directive), "=")[0]

	newContent := re.ReplaceAllStringFunc(templateContent, func(match string) string {
		parts := re.FindStringSubmatch(match)
		if len(parts) != 4 {
			return match
		}

		openTag := parts[1]
		attrs := parts[2]
		closeBracket := parts[3]

		if matchAttr != "" && !strings.Contains(attrs, matchAttr) {
			return match
		}

		if strings.Contains(attrs, directiveKey) {
			return match
		}

		changed = true
		injectedCount++
		return fmt.Sprintf("%s %s%s%s", openTag, directive, attrs, closeBracket)
	})

	if !changed {
		if matchAttr != "" {
			return SurgicalEditResult{}, fmt.Errorf("tag <%s> with attribute '%s' not found, or directive already exists", tag, matchAttr)
		}
		return SurgicalEditResult{}, fmt.Errorf("tag <%s> not found in template, or directive already exists", tag)
	}

	if dryRun {
		return SurgicalEditResult{
			File:    file,
			Symbol:  fmt.Sprintf("<%s>", tag),
			Changed: true,
			Findings: []Finding{{
				File:    file,
				Rule:    "frontend.inject_directive.dry_run",
				Message: fmt.Sprintf("would inject '%s' into %d <%s> element(s)", directive, injectedCount, tag),
			}},
		}, nil
	}

	blocks[templateBlockIndex].Content = newContent
	newSource := ReplaceSFCBlocks(src, blocks)

	if err := os.WriteFile(absPath, newSource, 0644); err != nil {
		return SurgicalEditResult{}, fmt.Errorf("write %s: %w", file, err)
	}

	_ = f.idx.IndexFile(file, LanguageVue)

	return SurgicalEditResult{
		File:    file,
		Symbol:  fmt.Sprintf("<%s>", tag),
		Changed: true,
		Findings: []Finding{{
			File:    file,
			Rule:    "frontend.inject_directive",
			Message: fmt.Sprintf("injected '%s' into %d <%s> element(s)", directive, injectedCount, tag),
		}},
	}, nil
}

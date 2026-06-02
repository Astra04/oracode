package oracode

import (
	"bytes"
	"fmt"
	"os"
	"strings"
)

// ReplaceBlock replaces an entire block (script, template, style) in a Vue SFC.
func (f *FrontendOps) ReplaceBlock(file, blockType, newContent string) error {
	absPath, err := f.idx.Policy.ResolveWorkspacePath(file)
	if err != nil {
		return err
	}
	src, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}
	blocks, err := ParseVueSFC(bytes.NewReader(src))
	if err != nil {
		return err
	}
	var targetIdx int = -1
	for i, b := range blocks {
		if b.Tag == blockType {
			targetIdx = i
			break
		}
	}
	if targetIdx == -1 {
		return fmt.Errorf("no <%s> block found in %s", blockType, file)
	}

	if newContent != "" && !strings.HasPrefix(newContent, "\n") {
		newContent = "\n" + newContent
	}
	if newContent != "" && !strings.HasSuffix(newContent, "\n") {
		newContent = newContent + "\n"
	}

	blocks[targetIdx].Content = newContent
	newSource := ReplaceSFCBlocks(src, blocks)
	if err := os.WriteFile(absPath, newSource, 0644); err != nil {
		return err
	}
	lang, _ := DetectOraLanguage(file)
	return f.idx.IndexFile(file, lang)
}

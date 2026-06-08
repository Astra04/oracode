package oracode

import (
	"bytes"
	"fmt"
	"os"
	"strings"
)

// ResolveBlockOffset translates block-relative lines into absolute file lines.
// It assumes 1-based startLine and endLine relative to the start of the block's content.
func ResolveBlockOffset(absPath string, blockType string, startLine, endLine int) (int, int, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return 0, 0, err
	}

	blocks, err := ParseVueSFC(bytes.NewReader(data))
	if err != nil {
		return 0, 0, fmt.Errorf("failed to parse SFC: %v", err)
	}

	var targetBlock *SFCBlock
	for i := range blocks {
		if blocks[i].Tag == blockType {
			targetBlock = &blocks[i]
			break
		}
	}

	if targetBlock == nil {
		return 0, 0, fmt.Errorf("block <%s> not found in file", blockType)
	}

		contentStartOffset := targetBlock.StartOffset + len(targetBlock.OpenTagRaw)

	// Count newlines before the content starts to find the 1-based absolute line number
	// where the content begins.
	linesBeforeContent := strings.Count(string(data[:contentStartOffset]), "\n")
	absContentStartLine := linesBeforeContent + 1

    // If the content itself starts with a newline, it means the content effectively
    // starts on the *next* line. If not, it shares the line with the opening tag.
    // However, usually we don't want to replace the line with the opening tag.
    // Let's rely on standard strings.Split logic. strings.Split does not split the \n inside the tag.

    // Actually, line 1 of the file is index 0.
    // linesBeforeContent = 0 means line 1.
    // Let's accurately determine which line the content starts on.
    // The opening tag ends at contentStartOffset.

    // If we replace lines, we use 1-based absolute line numbers.
    // We want line 1 of the block to correspond to the line AFTER the opening tag,
    // UNLESS there's content on the same line as the opening tag.

    // Let's use a simple heuristic: if there's a newline immediately following the opening tag,
    // then block line 1 is absContentStartLine + 1. Otherwise, it's absContentStartLine.

    startLineOffset := 0
    if len(targetBlock.Content) > 0 && targetBlock.Content[0] == '\n' {
        startLineOffset = 1
    }

	if startLine == 0 {
		startLine = 1 // default to first line of content
	}

	absStart := absContentStartLine + startLineOffset + startLine - 1

    contentLinesCount := strings.Count(targetBlock.Content, "\n")
    if startLineOffset == 0 && contentLinesCount == 0 {
        // entire block is on one line
        contentLinesCount = 1
    } else if startLineOffset == 1 {
        // if it starts with \n, that \n doesn't count as a line of content in itself,
        // but it means the first actual line of content is the next line.
        // strings.Split("\nhello\n", "\n") has 3 items.
    }

    // Better way: just map lines!
    // Let's count total lines in the file up to the end of the opening tag.
    // That's absContentStartLine.
    // Total lines up to the start of the closing tag.
    contentEndOffset := targetBlock.EndOffset - len(targetBlock.CloseTagRaw)
    linesBeforeEnd := strings.Count(string(data[:contentEndOffset]), "\n")
    _ = linesBeforeEnd + 1

    // So the content spans from absContentStartLine to absContentEndLine.
    // Example:
    // 1: <template>
    // 2:   <div>
    // 3:   </div>
    // 4: </template>
    // contentStartOffset is just after <template>. linesBeforeContent = 0. absContentStartLine = 1.
    // contentEndOffset is just before </template>. linesBeforeEnd = 3. absContentEndLine = 4.

    // To protect the tags, the actual replaceable lines are from
    // (absContentStartLine + 1) to (absContentEndLine - 1) if the tags are on their own lines.

    // Let's find exactly which line index the opening tag is on, and the closing tag is on.
    openTagLine := linesBeforeContent + 1
    closeTagLine := linesBeforeEnd + 1

    // The safe editable range is [openTagLine + 1, closeTagLine - 1]
    // IF openTagLine < closeTagLine. If they are on the same line, editable range is empty for full lines,
    // but range_replace replaces full lines, which would destroy the tags. So we shouldn't allow range_replace on single-line blocks unless we replace the whole block (which replaces tags too).
    // Actually, if we just offset from openTagLine + 1:

    blockStartLineAbs := openTagLine
    if len(targetBlock.Content) > 0 && targetBlock.Content[0] == '\n' {
        blockStartLineAbs = openTagLine + 1
    }

	if startLine == 0 {
		startLine = 1
	}

	absStart = blockStartLineAbs + startLine - 1

	var absEnd int
	if endLine == 0 {
		// up to the line before the close tag line
        // wait, if the content has a trailing newline, closeTagLine is the line with </template>.
        // So the last content line is closeTagLine - 1.
        if len(targetBlock.Content) > 0 && targetBlock.Content[len(targetBlock.Content)-1] == '\n' {
            absEnd = closeTagLine - 1
        } else {
            absEnd = closeTagLine
        }
	} else {
		absEnd = blockStartLineAbs + endLine - 1
	}

    // Safety checks
    if absStart <= openTagLine {
        // don't overwrite opening tag unless user explicitly forced something weird,
        // but actually, we should just clamp to protect it if it's on a separate line.
        // Wait, if startLine = 1, and blockStartLineAbs = openTagLine+1, absStart = openTagLine+1. Safe.
    }

    if absEnd >= closeTagLine && len(targetBlock.Content) > 0 && targetBlock.Content[len(targetBlock.Content)-1] == '\n' {
        absEnd = closeTagLine - 1 // clamp to protect closing tag
    }

	return absStart, absEnd, nil
}

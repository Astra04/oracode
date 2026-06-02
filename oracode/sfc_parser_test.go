package oracode

import (
	"bytes"
	"strings"
	"testing"
)

func TestReplaceSFCBlocksPreservesUnchangedSource(t *testing.T) {
	src := []byte(`<template><div>{{ id }}</div></template>

<script setup lang="ts">
const id = uuid.v4()
</script>

<style scoped>
div { color: red; }
</style>
`)

	blocks, err := ParseVueSFC(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("ParseVueSFC returned error: %v", err)
	}
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(blocks))
	}

	for i := range blocks {
		if blocks[i].Tag == "script" {
			blocks[i].Content = strings.ReplaceAll(blocks[i].Content, "uuid.v4()", "generate_pbuuid()")
		}
	}

	out := string(ReplaceSFCBlocks(src, blocks))
	if !strings.Contains(out, "const id = generate_pbuuid()") {
		t.Fatalf("expected script rewrite, got:\n%s", out)
	}
	if !strings.Contains(out, `<style scoped>`) {
		t.Fatalf("expected style block to survive, got:\n%s", out)
	}
}

// TestParseSFCLargeFile is a regression test for the slice-bounds panic
// (contentStart > contentEnd) that occurred on Vue files larger than ~16 KB.
// Root cause: tokenizer.Token() internally calls tagAttr() which advances the
// tokenizer's scan pointer, corrupting Raw()/Buffered() byte offsets when called
// after Token() instead of before it.
func TestParseSFCLargeFile(t *testing.T) {
	// Build a template block that is well over 16 KB to expose the offset bug.
	const rowCount = 400
	var tmpl strings.Builder
	tmpl.WriteString("<template>\n  <div class=\"container\">\n")
	for i := 0; i < rowCount; i++ {
		tmpl.WriteString("    <div class=\"row\">")
		tmpl.WriteString("      <span class=\"col-label\">Label for item number one two three four five six seven eight nine ten</span>\n")
		tmpl.WriteString("      <span class=\"col-value\">Value string with some realistic content to pad the file size adequately</span>\n")
		tmpl.WriteString("    </div>\n")
	}
	tmpl.WriteString("  </div>\n</template>\n\n")
	tmpl.WriteString("<script setup lang=\"ts\">\nconst marker = uuid.v4()\n</script>\n\n")
	tmpl.WriteString("<style scoped>\n.container { display: flex; }\n</style>\n")

	src := []byte(tmpl.String())
	if len(src) < 16000 {
		t.Fatalf("test setup error: source too small (%d bytes), increase rowCount", len(src))
	}

	blocks, err := ParseVueSFC(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("ParseVueSFC returned error on large file: %v", err)
	}
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(blocks))
	}

	for _, b := range blocks {
		if b.StartOffset < 0 || b.EndOffset > len(src) {
			t.Errorf("block %q offsets out of source range: start=%d end=%d srcLen=%d",
				b.Tag, b.StartOffset, b.EndOffset, len(src))
		}
		contentStart := b.StartOffset + len(b.OpenTagRaw)
		contentEnd := contentStart + len(b.Content)
		if contentEnd > b.EndOffset {
			t.Errorf("block %q content extends beyond EndOffset: contentEnd=%d EndOffset=%d",
				b.Tag, contentEnd, b.EndOffset)
		}
	}

	// Verify script content is intact and correct.
	var scriptBlock *SFCBlock
	for i := range blocks {
		if blocks[i].Tag == "script" {
			scriptBlock = &blocks[i]
		}
	}
	if scriptBlock == nil {
		t.Fatal("script block not found")
	}
	if !strings.Contains(scriptBlock.Content, "uuid.v4()") {
		t.Errorf("expected uuid.v4() in script content, got: %q", scriptBlock.Content)
	}

	// Verify ReplaceSFCBlocks round-trips cleanly on a large file.
	scriptBlock.Content = strings.ReplaceAll(scriptBlock.Content, "uuid.v4()", "generate_pbuuid()")
	out := string(ReplaceSFCBlocks(src, blocks))
	if !strings.Contains(out, "generate_pbuuid()") {
		t.Error("expected uuid rewrite to appear in output")
	}
	if strings.Contains(out, "uuid.v4()") {
		t.Error("original uuid.v4() still present after rewrite")
	}
}

// TestParseSFCMultiAttrOpenTag ensures offset calculation is correct when the
// opening tag carries multiple attributes (longer raw bytes), which was another
// path that could produce wrong contentStart values.
func TestParseSFCMultiAttrOpenTag(t *testing.T) {
	src := []byte(`<template id="root" data-v-app class="wrapper">
  <p>hello</p>
</template>

<script setup lang="ts" generic="T extends object">
export const foo = 1
</script>
`)

	blocks, err := ParseVueSFC(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("ParseVueSFC error: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	for _, b := range blocks {
		reconstructed := b.OpenTagRaw + b.Content + b.CloseTagRaw
		if reconstructed != b.OriginalText {
			t.Errorf("block %q: OpenTagRaw+Content+CloseTagRaw != OriginalText\ngot:  %q\nwant: %q",
				b.Tag, reconstructed, b.OriginalText)
		}
	}
}

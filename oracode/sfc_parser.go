package oracode

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"runtime/debug"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

type SFCBlock struct {
	Tag          string
	Attrs        map[string]string
	Content      string
	StartOffset  int
	EndOffset    int
	OpenTagRaw   string
	CloseTagRaw  string
	OriginalText string
}

// countingReader wraps an io.Reader and tracks the total bytes pulled out
// by the caller (the html.Tokenizer's internal bufio.Reader).
//
// Why this exists:
// html.Tokenizer wraps its input in a bufio.Reader that reads in chunks
// (typically 4096 bytes). At any point during tokenization, the bufio
// buffer holds only a slice of the full file — not all of it.
//
// The old offset formula was:
//
//	openStart = len(data) - len(buffered) - len(raw)
//
// This is wrong when len(data) > bytes actually read into the bufio buffer,
// because len(data) is always the full file size, making openStart too large
// (hence contentStart > contentEnd on larger Vue files).
//
// The correct formula is:
//
//	openStart = cr.total - len(buffered) - len(raw)
//
// where cr.total is the bytes actually pulled into the bufio buffer so far.
type countingReader struct {
	r     io.Reader
	total int
}

func (c *countingReader) Read(p []byte) (n int, err error) {
	n, err = c.r.Read(p)
	c.total += n
	return
}

func ParseVueSFC(r io.Reader) ([]SFCBlock, error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[sfc_parser] panic in ParseVueSFC: %v\n%s", r, debug.Stack())
		}
	}()

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read vue source: %w", err)
	}

	cr := &countingReader{r: bytes.NewReader(data)}
	tokenizer := html.NewTokenizer(cr)
	var blocks []SFCBlock

	for {
		tt := tokenizer.Next()
		if tt == html.ErrorToken {
			if tokenizer.Err() == io.EOF {
				break
			}
			return nil, fmt.Errorf("html tokenization failed: %w", tokenizer.Err())
		}

		if tt != html.StartTagToken {
			continue
		}

		// Snapshot Raw/Buffered/cr.total BEFORE Token() — Token() calls
		// tagAttr() internally which advances the tokenizer scan pointer.
		outerRaw := append([]byte(nil), tokenizer.Raw()...)
		outerBuffered := tokenizer.Buffered()
		outerTotal := cr.total
		token := tokenizer.Token()
		tag := strings.ToLower(token.Data)
		if tag != "template" && tag != "script" && tag != "style" {
			continue
		}

		openStart := outerTotal - len(outerBuffered) - len(outerRaw)
		contentStart := openStart + len(outerRaw)
		depth := 1

		var contentEnd int
		var closeRaw []byte
		for depth > 0 {
			innerTT := tokenizer.Next()
			if innerTT == html.ErrorToken {
				return nil, fmt.Errorf("unterminated <%s> block", tag)
			}

			innerRaw := append([]byte(nil), tokenizer.Raw()...)
			innerBuffered := tokenizer.Buffered()
			innerTotal := cr.total
			innerToken := tokenizer.Token()
			innerTag := strings.ToLower(innerToken.Data)
			if innerTT == html.StartTagToken && innerTag == tag {
				depth++
			}
			if innerTT == html.EndTagToken && innerTag == tag {
				depth--
				if depth == 0 {
					closeRaw = innerRaw
					contentEnd = innerTotal - len(innerBuffered) - len(innerRaw)
				}
			}
		}

		attrs := make(map[string]string, len(token.Attr))
		for _, attr := range token.Attr {
			attrs[strings.ToLower(attr.Key)] = attr.Val
		}

		endOffset := contentEnd + len(closeRaw)

		if contentStart < 0 || contentEnd < 0 ||
			contentStart > len(data) || contentEnd > len(data) ||
			openStart < 0 || endOffset < 0 ||
			openStart > len(data) || endOffset > len(data) {
			log.Printf("[sfc_parser] invalid slice bounds: openStart=%d, contentStart=%d, contentEnd=%d, endOffset=%d, dataLen=%d",
				openStart, contentStart, contentEnd, endOffset, len(data))
			continue
		}

		if contentStart > contentEnd {
			log.Printf("[sfc_parser] invalid range contentStart > contentEnd: %d > %d, skipping block",
				contentStart, contentEnd)
			continue
		}

		blocks = append(blocks, SFCBlock{
			Tag:          tag,
			Attrs:        attrs,
			Content:      string(data[contentStart:contentEnd]),
			StartOffset:  openStart,
			EndOffset:    endOffset,
			OpenTagRaw:   string(data[openStart:contentStart]),
			CloseTagRaw:  string(closeRaw),
			OriginalText: string(data[openStart:endOffset]),
		})
	}

	return blocks, nil
}

func ReplaceSFCBlocks(source []byte, blocks []SFCBlock) []byte {
	if len(blocks) == 0 {
		return source
	}

	sort.Slice(blocks, func(i, j int) bool {
		return blocks[i].StartOffset < blocks[j].StartOffset
	})

	var out strings.Builder
	last := 0
	for _, block := range blocks {
		out.Write(source[last:block.StartOffset])
		out.WriteString(block.OpenTagRaw)
		out.WriteString(block.Content)
		out.WriteString(block.CloseTagRaw)
		last = block.EndOffset
	}
	out.Write(source[last:])
	return []byte(out.String())
}

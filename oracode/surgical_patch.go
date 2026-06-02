package oracode

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/dave/dst"
)

func (s *SurgicalOps) PatchSymbolBody(file, symbolName, searchSnippet, replaceSnippet string) (SurgicalEditResult, error) {
	absPath, err := s.idx.Policy.ResolveWorkspacePath(file)
	if err != nil {
		return SurgicalEditResult{}, err
	}
	src, err := os.ReadFile(absPath)
	if err != nil {
		return SurgicalEditResult{}, fmt.Errorf("read %s: %w", file, err)
	}
	fileNode, err := SafeDstParse(src)
	if err != nil {
		return SurgicalEditResult{}, fmt.Errorf("parse %s: %w", file, err)
	}

	normalize := func(s string) string {
		s = strings.ReplaceAll(s, "\t", "    ")
		lines := strings.Split(s, "\n")
		var out []string
		for _, l := range lines {
			t := strings.TrimRight(l, " \r")
			if t != "" {
				out = append(out, t)
			}
		}
		return strings.Join(out, "\n")
	}

	var changed bool
	for _, decl := range fileNode.Decls {
		fn, ok := decl.(*dst.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != symbolName || fn.Body == nil {
			continue
		}

		// Wrap func body in a temp file for rendering
		dummyDecl := &dst.FuncDecl{
			Name: dst.NewIdent("_"),
			Type: &dst.FuncType{},
			Body: fn.Body,
		}
		dummyFile := &dst.File{
			Name:  dst.NewIdent("patch"),
			Decls: []dst.Decl{dummyDecl},
		}
		var buf bytes.Buffer
		if err := SafeDstFprint(&buf, dummyFile); err != nil {
			return SurgicalEditResult{}, err
		}
		// Extract just the body by removing the wrapper
		raw := buf.String()
		braceIdx := strings.Index(raw, "{")
		if braceIdx < 0 {
			return SurgicalEditResult{}, fmt.Errorf("cannot locate body braces in rendered output")
		}
		currentBodyStr := "{" + raw[braceIdx+1:len(raw)-1] + "}"

		normCurrent := normalize(currentBodyStr)
		normSearch := normalize(searchSnippet)

		if !strings.Contains(normCurrent, normSearch) {
			return SurgicalEditResult{}, fmt.Errorf("search snippet not found in %s", symbolName)
		}
		newBodyStr := strings.Replace(normCurrent, normSearch, replaceSnippet, 1)

		wrapped := "package patch\nfunc _() " + newBodyStr
		patchNode, err := SafeDstParse([]byte(wrapped))
		if err != nil {
			return SurgicalEditResult{}, fmt.Errorf("syntax error in replacement snippet: %w", err)
		}

		if patchFn, ok := patchNode.Decls[0].(*dst.FuncDecl); ok && patchFn.Body != nil {
			fn.Body = patchFn.Body
			changed = true
			break
		}
	}

	if !changed {
		return SurgicalEditResult{}, fmt.Errorf("symbol %q not found or unchanged", symbolName)
	}

	var buf bytes.Buffer
	if err := SafeDstFprint(&buf, fileNode); err != nil {
		return SurgicalEditResult{}, fmt.Errorf("print %s: %w", file, err)
	}
	if err := os.WriteFile(absPath, buf.Bytes(), 0o644); err != nil {
		return SurgicalEditResult{}, fmt.Errorf("write %s: %w", file, err)
	}

	return SurgicalEditResult{
		File:    file,
		Symbol:  symbolName,
		Changed: true,
		Findings: []Finding{{
			File:    file,
			Rule:    "surgical.patch_symbol",
			Message: fmt.Sprintf("patched block inside %q", symbolName),
		}},
	}, nil
}

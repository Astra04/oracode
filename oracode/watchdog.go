package oracode

import (
	"bufio"
	"os"
	"regexp"
	"strconv"
)

type ErrorMatch struct {
	Line    string `json:"line"`
	File    string `json:"file"`
	LineNum int    `json:"lineNum"`
	Symbol  string `json:"symbol"`
}

func (idx *Index) ScanErrors(logPath string, limit int) ([]ErrorMatch, error) {
	file, err := os.Open(logPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// Regex to capture file:line from Go stack traces
	re := regexp.MustCompile(`(\S+\.go):(\d+)`)
	var matches []ErrorMatch

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		sub := re.FindStringSubmatch(line)
		if len(sub) == 3 {
			fileMatch := sub[1]
			lineNum, _ := strconv.Atoi(sub[2])
			sym := idx.findSymbolAtLocation(fileMatch, lineNum)
			matches = append(matches, ErrorMatch{
				Line:    line,
				File:    fileMatch,
				LineNum: lineNum,
				Symbol:  sym,
			})
			if len(matches) >= limit {
				break
			}
		}
	}
	return matches, scanner.Err()
}

// findSymbolAtLocation returns the symbol name that encloses the given line.
func (idx *Index) findSymbolAtLocation(file string, line int) string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	for _, syms := range idx.FileSymbols {
		for _, sym := range syms {
			if sym.File == file && sym.Line <= line && line-sym.Line < 20 {
				return sym.Name
			}
		}
	}
	return ""
}

package oracode

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Engine struct {
	WorkspaceRoot string
	DryRun        bool
	Security      SecurityPolicy
}

func NewEngine(root string) *Engine {
	clean := filepath.Clean(root)
	return &Engine{
		WorkspaceRoot: clean,
		Security:      DefaultSecurityPolicy(clean),
	}
}

func (e *Engine) ProcessFile(filePath string) (Report, error) {
	absPath, err := e.resolve(filePath)
	if err != nil {
		return Report{}, err
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return Report{}, fmt.Errorf("read %s: %w", filePath, err)
	}

	switch strings.ToLower(filepath.Ext(filePath)) {
	case ".go":
		return e.processGo(filePath, absPath, data)
	case ".vue":
		return e.processVue(filePath, absPath, data)
	default:
		return Report{File: filePath}, nil
	}
}

func (e *Engine) processGo(filePath, absPath string, data []byte) (Report, error) {
	mutated, err := MutateGoSource(filePath, data)
	if err != nil {
		return Report{}, err
	}
	report := Report{File: filePath, Changed: mutated.Changed, Findings: mutated.Findings}
	if mutated.Changed && !e.DryRun {
		if err := os.WriteFile(absPath, mutated.Source, 0o644); err != nil {
			return Report{}, fmt.Errorf("write %s: %w", filePath, err)
		}
	}
	return report, nil
}

func (e *Engine) processVue(filePath, absPath string, data []byte) (Report, error) {
	blocks, err := ParseVueSFC(bytes.NewReader(data))
	if err != nil {
		return Report{}, err
	}

	report := Report{File: filePath}
	for i := range blocks {
		block := &blocks[i]
		if block.Tag != "script" {
			continue
		}
		lang := strings.ToLower(block.Attrs["lang"])
		if lang != "ts" && lang != "tsx" {
			continue
		}

		imports, err := FindTypeScriptImports([]byte(block.Content), lang == "tsx")
		if err != nil {
			report.Findings = append(report.Findings, Finding{
				File:    filePath,
				Rule:    "vue.script.parse",
				Message: err.Error(),
			})
			continue
		}
		if len(imports) > 0 {
			report.Findings = append(report.Findings, Finding{
				File:    filePath,
				Rule:    "ts.imports",
				Message: fmt.Sprintf("found %d named TypeScript imports", len(imports)),
			})
		}

		rewritten := strings.ReplaceAll(block.Content, "uuid.v4()", "generate_pbuuid()")
		if rewritten != block.Content {
			block.Content = rewritten
			report.Changed = true
			report.Findings = append(report.Findings, Finding{
				File:    filePath,
				Rule:    "db.pbuuid_client",
				Message: "rewrote uuid.v4() to generate_pbuuid() inside Vue script block",
			})
		}
	}

	if report.Changed && !e.DryRun {
		if err := os.WriteFile(absPath, ReplaceSFCBlocks(data, blocks), 0o644); err != nil {
			return Report{}, fmt.Errorf("write %s: %w", filePath, err)
		}
	}
	return report, nil
}

func (e *Engine) VerifyWorkspace() (bool, string) {
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = e.WorkspaceRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, string(output)
	}
	return true, "OraCode verification passed: go build ./..."
}

func (e *Engine) resolve(filePath string) (string, error) {
	return e.Security.ResolveWorkspacePath(filePath)
}

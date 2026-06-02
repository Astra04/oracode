package oracode

import (
	"fmt"
	"strings"
)

type Finding struct {
	File    string
	Rule    string
	Message string
}

type Report struct {
	File     string
	Changed  bool
	Findings []Finding
}

func (r Report) String() string {
	var out strings.Builder
	if r.File != "" {
		status := "unchanged"
		if r.Changed {
			status = "changed"
		}
		fmt.Fprintf(&out, "OraCode %s: %s\n", status, r.File)
	}
	for _, f := range r.Findings {
		fmt.Fprintf(&out, "- [%s] %s: %s\n", f.Rule, f.File, f.Message)
	}
	return out.String()
}

package oracode

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Policy struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Language    string `yaml:"language"`
	Pattern     string `yaml:"pattern"`
	Message     string `yaml:"message"`
}

func (idx *Index) RunPolicy(policyPath string) ([]string, error) {
	data, err := os.ReadFile(policyPath)
	if err != nil {
		return nil, err
	}

	var policies []Policy
	if err := yaml.Unmarshal(data, &policies); err != nil {
		// Fallback to single policy unmarshal
		var single Policy
		if err := yaml.Unmarshal(data, &single); err != nil {
			return nil, err
		}
		if single.Name != "" || single.Pattern != "" {
			policies = append(policies, single)
		}
	}

	var violations []string
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	for rel := range idx.Files {
		if !strings.HasSuffix(rel, ".go") {
			continue
		}
		abs, err := idx.Policy.ResolveWorkspacePath(rel)
		if err != nil {
			continue
		}
		src, err := os.ReadFile(abs)
		if err != nil {
			continue
		}

		srcStr := string(src)
		for _, policy := range policies {
			if policy.Language != "go" {
				continue
			}
			if policy.Pattern == "" {
				continue
			}
			if strings.Contains(srcStr, policy.Pattern) {
				violations = append(violations, fmt.Sprintf("%s: pattern %q found in %s - %s", policy.Name, policy.Pattern, rel, policy.Message))
			}
		}
	}
	return violations, nil
}

package oracode

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
)

type Constraint struct {
	ID       string `yaml:"id"`
	Name     string `yaml:"name"`
	Pattern  string `yaml:"pattern"`
	Required bool   `yaml:"required"`
	Message  string `yaml:"message"`
	Language string `yaml:"language"`
}

// LoadConstraints reads constraints.yaml from .oracode directory.
func LoadConstraints(workspaceRoot string) ([]Constraint, error) {
	path := filepath.Join(workspaceRoot, ".oracode", "constraints.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read constraints: %w", err)
	}

	// Try unmarshaling as a map containing a 'rules' key first
	var wrapper struct {
		Rules []Constraint `yaml:"rules"`
	}
	if err := yaml.Unmarshal(data, &wrapper); err == nil {
		return wrapper.Rules, nil
	}

	// Fallback: unmarshal directly as a top-level slice
	var constraints []Constraint
	if err := yaml.Unmarshal(data, &constraints); err != nil {
		return nil, fmt.Errorf("parse constraints: %w", err)
	}
	return constraints, nil
}

// matchConstraint tests one constraint against a code snippet.
// Returns (matched bool, error).
func matchConstraint(c Constraint, code string) (bool, error) {
	return regexp.MatchString(c.Pattern, code)
}

// ValidateCode checks a code snippet against constraints for a given language.
func ValidateCode(code, language string, constraints []Constraint) []string {
	var violations []string
	for _, c := range constraints {
		if c.Language != language {
			continue
		}
		matched, err := matchConstraint(c, code)
		if err != nil {
			continue
		}
		if c.Required && !matched {
			violations = append(violations, fmt.Sprintf("%s: %s", c.ID, c.Message))
		} else if !c.Required && matched {
			violations = append(violations, fmt.Sprintf("%s: %s (should not match)", c.ID, c.Message))
		}
	}
	return violations
}


// Violation describes a single constraint check failure.
type Violation struct {
	ConstraintID string `json:"constraint_id"`
	Message      string `json:"message"`
	Line         int    `json:"line,omitempty"`
}

// ValidationResult holds the outcome of a ValidateCode run.
type ValidationResult struct {
	Valid      bool        `json:"valid"`
	Violations []Violation `json:"violations,omitempty"`
}

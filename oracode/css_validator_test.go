package oracode

import "testing"

func TestValidateCSS(t *testing.T) {
	tests := []struct {
		name       string
		css        string
		wantErrors int
	}{
		{
			name: "Valid CSS",
			css: `.test-container {
				background: #fff;
			}`,
			wantErrors: 0,
		},
		{
			name: "Unclosed brace",
			css: `.test-container {
				background: #fff;
				unclosed: true;`,
			wantErrors: 1,
		},
		{
			name: "Unexpected closing brace",
			css: `.test-container {
				background: #fff;
			}
			}`,
			wantErrors: 1,
		},
		{
			name: "Valid CSS with strings",
			css: `.icon::before {
				content: "}";
			}
			.icon::after {
				content: '{';
			}`,
			wantErrors: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			violations := ValidateCSS(tt.css, "test.css")
			if len(violations) != tt.wantErrors {
				t.Errorf("ValidateCSS() got %v errors, want %v. Violations: %v", len(violations), tt.wantErrors, violations)
			}
		})
	}
}

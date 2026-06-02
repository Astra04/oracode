package oracode

import "testing"

func TestParserPoolParsesPhaseOneLanguages(t *testing.T) {
	pool, err := NewParserPool()
	if err != nil {
		t.Fatalf("NewParserPool returned error: %v", err)
	}

	cases := []struct {
		name string
		lang Language
		src  string
	}{
		{name: "go", lang: LanguageGo, src: "package demo\nfunc CreateOrder() {}\n"},
		{name: "javascript", lang: LanguageJavaScript, src: "export function useFeatureFlag() { return true }\n"},
		{name: "typescript", lang: LanguageTypeScript, src: "export const total: number = 1\n"},
		{name: "tsx", lang: LanguageTSX, src: "export const View = () => <div />\n"},
		{name: "vue", lang: LanguageVue, src: "<template><div>{{ id }}</div></template><script setup lang=\"ts\">const id = 1</script>\n"},
		{name: "sql", lang: LanguageSQL, src: "CREATE TABLE orders (id text primary key);\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cst, err := pool.ParseSource(tc.name, tc.lang, []byte(tc.src))
			if err != nil {
				t.Fatalf("ParseSource returned error: %v", err)
			}
			defer cst.Release()
			if cst.RootType() == "" {
				t.Fatal("expected root type")
			}
		})
	}
}

func TestSecurityPolicyRejectsTraversal(t *testing.T) {
	policy := DefaultSecurityPolicy(".")
	if _, err := policy.ResolveWorkspacePath("..\\outside.go"); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
}

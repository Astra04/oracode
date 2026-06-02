package oracode

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

type Language string

const (
	LanguageGo         Language = "go"
	LanguageJavaScript Language = "javascript"
	LanguageTypeScript Language = "typescript"
	LanguageTSX        Language = "tsx"
	LanguageVue        Language = "vue"
	LanguageSQL        Language = "sql"
)

var languageByExtension = map[string]Language{
	".go":  LanguageGo,
	".js":  LanguageJavaScript,
	".jsx": LanguageJavaScript,
	".ts":  LanguageTypeScript,
	".tsx": LanguageTSX,
	".vue": LanguageVue,
	".sql": LanguageSQL,
}

func DetectOraLanguage(path string) (Language, bool) {
	lang, ok := languageByExtension[strings.ToLower(filepath.Ext(path))]
	return lang, ok
}

func SupportedLanguages() []Language {
	return []Language{
		LanguageGo,
		LanguageJavaScript,
		LanguageTypeScript,
		LanguageTSX,
		LanguageVue,
		LanguageSQL,
	}
}

func LanguageForName(name string) (Language, error) {
	normalized := strings.ToLower(strings.TrimSpace(name))
	switch Language(normalized) {
	case LanguageGo, LanguageJavaScript, LanguageTypeScript, LanguageTSX, LanguageVue, LanguageSQL:
		return Language(normalized), nil
	case "js":
		return LanguageJavaScript, nil
	case "ts":
		return LanguageTypeScript, nil
	default:
		return "", fmt.Errorf("unsupported language: %s", name)
	}
}

func grammarForLanguage(lang Language) (*gotreesitter.Language, error) {
	switch lang {
	case LanguageGo:
		return grammars.GoLanguage(), nil
	case LanguageJavaScript:
		return grammars.JavascriptLanguage(), nil
	case LanguageTypeScript:
		return grammars.TypescriptLanguage(), nil
	case LanguageTSX:
		return grammars.TsxLanguage(), nil
	case LanguageVue:
		return grammars.VueLanguage(), nil
	case LanguageSQL:
		return grammars.SqlLanguage(), nil
	default:
		return nil, fmt.Errorf("unsupported language: %s", lang)
	}
}

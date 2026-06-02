package oracode

import (
	"fmt"
	"os"
	"sync"

	"github.com/odvcencio/gotreesitter"
)

const DefaultParseTimeoutMicros uint64 = 750_000

type CST struct {
	Path     string
	Language Language
	Source   []byte
	Tree     *gotreesitter.BoundTree
}

func (c *CST) Release() {
	if c != nil && c.Tree != nil {
		c.Tree.Release()
	}
}

func (c *CST) RootType() string {
	if c == nil || c.Tree == nil || c.Tree.RootNode() == nil {
		return ""
	}
	return c.Tree.RootNode().Type(c.Tree.Language())
}

func (c *CST) HasError() bool {
	return c != nil && c.Tree != nil && c.Tree.RootNode() != nil && c.Tree.RootNode().HasError()
}

func (c *CST) SExpr() string {
	if c == nil || c.Tree == nil || c.Tree.RootNode() == nil {
		return ""
	}
	return c.Tree.RootNode().SExpr(c.Tree.Language())
}

type ParserPool struct {
	mu    sync.RWMutex
	pools map[Language]*gotreesitter.ParserPool
}

func NewParserPool() (*ParserPool, error) {
	pp := &ParserPool{pools: make(map[Language]*gotreesitter.ParserPool)}
	for _, lang := range SupportedLanguages() {
		if err := pp.ensure(lang); err != nil {
			return nil, err
		}
	}
	return pp, nil
}

func (p *ParserPool) ParseFile(path string, lang Language, policy SecurityPolicy) (*CST, error) {
	if lang == "" {
		detected, ok := DetectOraLanguage(path)
		if !ok {
			return nil, fmt.Errorf("cannot detect language for %s", path)
		}
		lang = detected
	}

	absPath, err := policy.ResolveWorkspacePath(path)
	if err != nil {
		return nil, err
	}
	source, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return p.ParseSource(path, lang, source)
}

func (p *ParserPool) ParseSource(path string, lang Language, source []byte) (*CST, error) {
	if p == nil {
		return nil, fmt.Errorf("parser pool is nil")
	}
	if err := p.ensure(lang); err != nil {
		return nil, err
	}

	p.mu.RLock()
	pool := p.pools[lang]
	p.mu.RUnlock()
	if pool == nil {
		return nil, fmt.Errorf("parser pool missing for %s", lang)
	}

	tree, err := pool.Parse(source)
	if err != nil {
		return nil, fmt.Errorf("parse %s as %s: %w", path, lang, err)
	}
	return &CST{
		Path:     path,
		Language: lang,
		Source:   append([]byte(nil), source...),
		Tree:     gotreesitter.Bind(tree),
	}, nil
}

func (p *ParserPool) ensure(lang Language) error {
	p.mu.RLock()
	_, ok := p.pools[lang]
	p.mu.RUnlock()
	if ok {
		return nil
	}

	grammar, err := grammarForLanguage(lang)
	if err != nil {
		return err
	}
	if grammar == nil {
		return fmt.Errorf("grammar unavailable for %s", lang)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.pools[lang]; !ok {
		p.pools[lang] = gotreesitter.NewParserPool(
			grammar,
			gotreesitter.WithParserPoolTimeoutMicros(DefaultParseTimeoutMicros),
		)
	}
	return nil
}

func ParseFile(path string, lang Language, policy SecurityPolicy) (*CST, error) {
	pool, err := NewParserPool()
	if err != nil {
		return nil, err
	}
	return pool.ParseFile(path, lang, policy)
}

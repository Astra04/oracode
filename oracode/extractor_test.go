package oracode

import (
	"testing"
)

func TestExtractVueScriptSymbolsAbsoluteLineNumbers(t *testing.T) {
	src := []byte(`<template>
</template>

<script setup lang="ts">
export const foo = 1
const bar = 2
</script>
`)

	cst := &CST{Path: "Component.vue", Language: LanguageVue, Source: src}
	syms := extractVueScriptSymbols(cst)
	if len(syms) < 2 {
		t.Fatalf("expected at least 2 script symbols, got %d", len(syms))
	}

	lineMap := map[string]int{}
	for _, sym := range syms {
		lineMap[sym.Name] = sym.Line
	}

	if got, want := lineMap["foo"], 5; got != want {
		t.Fatalf("foo line = %d, want %d", got, want)
	}
	if got, want := lineMap["bar"], 6; got != want {
		t.Fatalf("bar line = %d, want %d", got, want)
	}
}

package oracode

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveBlockOffset(t *testing.T) {
	src := `<template>
  <div>
    hello
  </div>
</template>
<script>
  console.log("hello");
</script>
<style>
.a { color: red; }
</style>
`
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.vue")
	os.WriteFile(path, []byte(src), 0644)

	// Test template block
    // lines:
    // 1: <template>
    // 2:   <div>
    // 3:     hello
    // 4:   </div>
    // 5: </template>
    // 6: <script>
    // 7:   console.log("hello");
    // 8: </script>
    // 9: <style>
    // 10: .a { color: red; }
    // 11: </style>
    // 12:
	start, end, err := ResolveBlockOffset(path, "template", 0, 0)
    if err != nil {
        t.Fatal(err)
    }
    if start != 2 || end != 4 {
        t.Fatalf("template 0,0 -> %d, %d; expected 2, 4", start, end)
    }

    start, end, err = ResolveBlockOffset(path, "script", 0, 0)
    if start != 7 || end != 7 {
        t.Fatalf("script 0,0 -> %d, %d; expected 7, 7", start, end)
    }

    // Specific line
    start, end, err = ResolveBlockOffset(path, "template", 2, 2)
    if start != 3 || end != 3 {
        t.Fatalf("template 2,2 -> %d, %d; expected 3, 3", start, end)
    }
}

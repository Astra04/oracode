package oracode

import (
	"strings"
	"testing"
)

func TestMutateGoSourcePocketBaseID(t *testing.T) {
	src := []byte(`package demo

import "github.com/google/uuid"

type Order struct {
	// stable public id
	ID uuid.UUID ` + "`json:\"id\"`" + `
}
`)

	result, err := MutateGoSource("order.go", src)
	if err != nil {
		t.Fatalf("MutateGoSource returned error: %v", err)
	}
	if !result.Changed {
		t.Fatal("expected source to change")
	}

	out := string(result.Source)
	if !strings.Contains(out, "ID string `db:\"id\" json:\"id\"`") {
		t.Fatalf("expected standardized ID field, got:\n%s", out)
	}
	if !strings.Contains(out, "// stable public id") {
		t.Fatalf("expected comment preservation, got:\n%s", out)
	}
}

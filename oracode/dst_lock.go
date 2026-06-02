package oracode

import (
	"io"
	"sync"

	"github.com/dave/dst"
	"github.com/dave/dst/decorator"
)

// dstMu synchronises all Go AST parsing and printing operations using the github.com/dave/dst package.
// The dave/dst decorator package has internal state and caches that are NOT concurrent-safe.
// Accessing it concurrently from multiple background goroutines (such as during re-indexing or
// parallel AST editing) causes nil pointer dereference panics on go/token.
var dstMu sync.Mutex

// SafeDstParse wraps decorator.Parse in a thread-safe mutex lock.
func SafeDstParse(src []byte) (*dst.File, error) {
	dstMu.Lock()
	defer dstMu.Unlock()
	return decorator.Parse(src)
}

// SafeDstFprint wraps decorator.Fprint in a thread-safe mutex lock.
func SafeDstFprint(w io.Writer, file *dst.File) error {
	dstMu.Lock()
	defer dstMu.Unlock()
	return decorator.Fprint(w, file)
}

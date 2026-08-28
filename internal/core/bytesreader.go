package core

import (
	"bytes"
	"io"
)

// newBytesReader exists so value.go can use a streaming decoder without importing
// bytes at the top of a file that is otherwise pure domain logic.
func newBytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

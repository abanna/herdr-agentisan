package store

import (
	"io"
	"time"
)

// NewULID is the id Send gives a message made at at, with entropy's next ten
// bytes as its random part.
func NewULID(at time.Time, entropy io.Reader) (string, error) { return newULID(at, entropy) }

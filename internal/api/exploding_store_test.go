package api_test

import (
	"context"
	"errors"

	"github.com/nerds-run/go-agents/internal/notes"
)

// errBoom carries a message that must never reach a client. The tests assert
// the handler replaces it with a generic 500 body.
var errBoom = errors.New("dial tcp: bad database credentials")

// explodingStore fails every operation with an error the handler does not
// recognise, exercising the default branch of the error mapping.
type explodingStore struct{}

var _ notes.Store = explodingStore{}

func (explodingStore) Create(context.Context, notes.Draft) (notes.Note, error) {
	return notes.Note{}, errBoom
}

func (explodingStore) Get(context.Context, string) (notes.Note, error) {
	return notes.Note{}, errBoom
}
func (explodingStore) List(context.Context) ([]notes.Note, error) { return nil, errBoom }
func (explodingStore) Delete(context.Context, string) error       { return errBoom }

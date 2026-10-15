package main

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is a Store's answer for a key it does not hold. Anything
// else -- an AccessDenied above all -- is an error, never a miss: a role
// that lost its grant must not turn every request into an upstream fetch
// that can then never be stored.
var ErrNotFound = errors.New("not found")

// Store is the object store the cache keeps its index in. The service
// holds no state of its own: every object lives here, under the key the
// front-end derived from the request.
type Store interface {
	// Head returns the size of the object at key, or ErrNotFound.
	Head(ctx context.Context, key string) (int64, error)
	// Get returns the object at key, or ErrNotFound.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Put stores everything r yields under key, and COMMITS IT ONLY WHEN
	// r ends in io.EOF. Any other error from r discards the write: a
	// half-written object must never become visible under key, because a
	// truncated object would then be served as a hit forever. Put reads r
	// from the calling goroutine only.
	Put(ctx context.Context, key string, r io.Reader, contentType string) (int64, error)
	// Presign returns a URL anyone may GET the object at key with, until
	// ttl has passed.
	Presign(ctx context.Context, key string, ttl time.Duration) (string, error)
}

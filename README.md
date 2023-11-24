# sylr.dev/btree/v2 [![Go Reference](https://pkg.go.dev/badge/sylr.dev/btree/v2.svg)](https://pkg.go.dev/sylr.dev/btree/v2) [![codecov](https://codecov.io/gh/sylr/go-btree/graph/badge.svg?token=V222K9OAA4)](https://codecov.io/gh/sylr/go-btree)

This package provides an in-memory B-Tree implementation for Go, useful as an
ordered, mutable data structure.

The API is based off of the wonderful https://pkg.go.dev/github.com/petar/GoLLRB/llrb,
and is meant to allow btree to act as a drop-in replacement for gollrb trees.

See https://pkg.go.dev/sylr.dev/btree/v2 for documentation.

## Fork disclaimer

`sylr.dev/btree/v2` is a fork of [`github.com/google/btree`](https://github.com/google/btree)
with the following adaptations:

- The copy-on-write mechanism has been removed
- The non-generic implementation has been removed
- The generic implementation now rely on an `Item[T]` interface.

```golang
type Item[T any] interface {
	Less(T) bool
	DeepCopy() T
}
```

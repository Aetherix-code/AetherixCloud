// Package store defines the replicated state store used by every other
// package, plus its implementations:
//
//   - SQLite (pure Go, modernc): used for unit tests (":memory:") and to run
//     a single node without Corrosion (OpenSQLite).
//   - Corrosion sidecar (OpenCorrosion): the production CRDT backend. The SQL
//     dialect is identical, so both share one implementation (sqlStore) over
//     a small backend interface.
//
// Every table has the shape
//
//	id TEXT PRIMARY KEY, <index columns...>, data TEXT, updated_at INTEGER
//
// where data holds protojson (for API resources) or JSON (for internal rows)
// and the index columns are denormalised for filtering. Writes are
// last-write-wins per row. Anything needing exactly-one-actor semantics must
// use internal/lease on top of the store.
package store

import (
	"context"
	"errors"
)

// ErrNotFound is returned by Get when the row does not exist.
var ErrNotFound = errors.New("store: not found")

// Record is one stored row.
type Record struct {
	ID string
	// Idx holds the values of the table's index columns.
	Idx map[string]string
	// Data is the serialized payload.
	Data []byte
	// UpdatedAt is unix nanoseconds assigned by the store on write.
	UpdatedAt int64
}

// Query filters List results. Where keys must be index columns of the table.
type Query struct {
	Where map[string]string
	// OrderBy is "id" (default) or "updated_at".
	OrderBy string
	Desc    bool
	Limit   int
}

// ChangeOp is the kind of a Change.
type ChangeOp string

const (
	OpPut    ChangeOp = "put"
	OpDelete ChangeOp = "delete"
)

// Change is delivered by Watch.
type Change struct {
	Table  string
	Op     ChangeOp
	ID     string
	Record Record // zero for deletes
}

// TableRecord pairs a record with its table for batched writes.
type TableRecord struct {
	Table  string
	Record Record
	Delete bool
}

// Store is the replicated state store. All implementations are safe for
// concurrent use.
type Store interface {
	// Migrate applies pending schema migrations (idempotent).
	Migrate(ctx context.Context) error
	// Put upserts a row (last write wins).
	Put(ctx context.Context, table string, r Record) error
	// Apply writes several rows atomically (best effort across nodes).
	Apply(ctx context.Context, ops []TableRecord) error
	// Get returns ErrNotFound if absent.
	Get(ctx context.Context, table, id string) (Record, error)
	List(ctx context.Context, table string, q Query) ([]Record, error)
	Delete(ctx context.Context, table, id string) error
	// Watch streams changes to a table (local and, for Corrosion, remote
	// writes) until ctx is done. Slow consumers may miss events: treat the
	// stream as a hint to re-List.
	Watch(ctx context.Context, table string) (<-chan Change, error)
	Close() error
}

package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

type sqliteBackend struct{ db *sql.DB }

// OpenSQLite opens (and migrates on Migrate) a SQLite database. Use ":memory:"
// for tests. A file path gives single-node operation without Corrosion.
func OpenSQLite(path string) (Store, error) {
	dsn := path
	if path == ":memory:" || path == "" {
		dsn = "file:mem" + fmt.Sprint(memCounter()) + "?mode=memory&cache=shared&_pragma=busy_timeout(5000)"
	} else {
		dsn = "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // serialise: simple and avoids SQLITE_BUSY
	s := newSQLStore(&sqliteBackend{db: db})
	s.migrate = func(ctx context.Context) error { return migrateSQLite(ctx, db) }
	return s, nil
}

// NewMemory returns an isolated in-memory store (already migrated).
func NewMemory() Store {
	s, err := OpenSQLite(":memory:")
	if err != nil {
		panic(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		panic(err)
	}
	return s
}

func migrateSQLite(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	for _, m := range Migrations() {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, m.Version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, s := range m.SQL {
			if _, err := tx.ExecContext(ctx, s); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d: %w", m.Version, err)
			}
		}
		// Secondary indexes (SQLite only; Corrosion schema has none).
		if m.Version == 1 {
			for _, t := range Tables {
				for _, c := range t.Indexes {
					if _, err := tx.ExecContext(ctx, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_%s_%s ON %s(%s)`, t.Name, c, t.Name, quoteIdent(c))); err != nil {
						tx.Rollback()
						return err
					}
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,applied_at) VALUES (?,?,strftime('%s','now'))`, m.Version, m.Name); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (b *sqliteBackend) exec(ctx context.Context, stmts []stmt) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s.sql, s.args...); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (b *sqliteBackend) query(ctx context.Context, q string, args ...any) ([][]any, error) {
	rows, err := b.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, vals)
	}
	return out, rows.Err()
}

func (b *sqliteBackend) subscribe(context.Context, string) (<-chan Change, error) { return nil, nil }
func (b *sqliteBackend) close() error                                             { return b.db.Close() }

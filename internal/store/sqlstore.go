package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

type stmt struct {
	sql  string
	args []any
}

// backend executes SQL. Rows are returned as [][]any with int64/string/[]byte
// normalised by the backend.
type backend interface {
	exec(ctx context.Context, stmts []stmt) error
	query(ctx context.Context, sql string, args ...any) ([][]any, error)
	// subscribe optionally streams remote changes; return (nil, nil) if the
	// backend only has local writes (which sqlStore publishes itself).
	subscribe(ctx context.Context, table string) (<-chan Change, error)
	close() error
}

type sqlStore struct {
	b backend

	mu       sync.Mutex
	watchers map[string]map[*watcher]struct{}
	// migrate controls whether Migrate runs DDL (false for Corrosion).
	migrate func(ctx context.Context) error
}

type watcher struct {
	ch chan Change
}

func newSQLStore(b backend) *sqlStore {
	return &sqlStore{b: b, watchers: map[string]map[*watcher]struct{}{}}
}

func (s *sqlStore) Migrate(ctx context.Context) error {
	if s.migrate != nil {
		return s.migrate(ctx)
	}
	return nil
}

func (s *sqlStore) Close() error { return s.b.close() }

func (s *sqlStore) putStmt(table string, r Record) (stmt, error) {
	def, ok := tableDef(table)
	if !ok {
		return stmt{}, fmt.Errorf("store: unknown table %q", table)
	}
	if r.ID == "" {
		return stmt{}, fmt.Errorf("store: empty id for %s", table)
	}
	cols := []string{"id"}
	args := []any{r.ID}
	for _, c := range def.Indexes {
		cols = append(cols, quoteIdent(c))
		args = append(args, r.Idx[c])
	}
	cols = append(cols, "data", "updated_at")
	args = append(args, string(r.Data), r.UpdatedAt)
	ph := strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")
	var sets []string
	for _, c := range cols[1:] {
		sets = append(sets, fmt.Sprintf("%s=excluded.%s", c, c))
	}
	q := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT(id) DO UPDATE SET %s",
		table, strings.Join(cols, ","), ph, strings.Join(sets, ","))
	return stmt{q, args}, nil
}

func (s *sqlStore) Put(ctx context.Context, table string, r Record) error {
	return s.Apply(ctx, []TableRecord{{Table: table, Record: r}})
}

func (s *sqlStore) Apply(ctx context.Context, ops []TableRecord) error {
	var stmts []stmt
	now := time.Now().UnixNano()
	for i := range ops {
		op := &ops[i]
		if op.Delete {
			if _, ok := tableDef(op.Table); !ok {
				return fmt.Errorf("store: unknown table %q", op.Table)
			}
			stmts = append(stmts, stmt{fmt.Sprintf("DELETE FROM %s WHERE id = ?", op.Table), []any{op.Record.ID}})
			continue
		}
		op.Record.UpdatedAt = now
		st, err := s.putStmt(op.Table, op.Record)
		if err != nil {
			return err
		}
		stmts = append(stmts, st)
	}
	if err := s.b.exec(ctx, stmts); err != nil {
		return err
	}
	for _, op := range ops {
		ch := Change{Table: op.Table, ID: op.Record.ID, Op: OpPut, Record: op.Record}
		if op.Delete {
			ch.Op = OpDelete
			ch.Record = Record{}
		}
		s.publish(ch)
	}
	return nil
}

func (s *sqlStore) Delete(ctx context.Context, table, id string) error {
	return s.Apply(ctx, []TableRecord{{Table: table, Record: Record{ID: id}, Delete: true}})
}

func selectCols(def TableDef) string {
	cols := []string{"id"}
	for _, c := range def.Indexes {
		cols = append(cols, quoteIdent(c))
	}
	cols = append(cols, "data", "updated_at")
	return strings.Join(cols, ",")
}

func rowToRecord(def TableDef, row []any) (Record, error) {
	if len(row) != len(def.Indexes)+3 {
		return Record{}, fmt.Errorf("store: unexpected column count %d", len(row))
	}
	r := Record{ID: toString(row[0])}
	if len(def.Indexes) > 0 {
		r.Idx = make(map[string]string, len(def.Indexes))
	}
	for i, c := range def.Indexes {
		r.Idx[c] = toString(row[1+i])
	}
	r.Data = []byte(toString(row[len(row)-2]))
	r.UpdatedAt = toInt64(row[len(row)-1])
	return r, nil
}

func (s *sqlStore) Get(ctx context.Context, table, id string) (Record, error) {
	def, ok := tableDef(table)
	if !ok {
		return Record{}, fmt.Errorf("store: unknown table %q", table)
	}
	rows, err := s.b.query(ctx, fmt.Sprintf("SELECT %s FROM %s WHERE id = ?", selectCols(def), table), id)
	if err != nil {
		return Record{}, err
	}
	if len(rows) == 0 {
		return Record{}, ErrNotFound
	}
	return rowToRecord(def, rows[0])
}

func (s *sqlStore) List(ctx context.Context, table string, q Query) ([]Record, error) {
	def, ok := tableDef(table)
	if !ok {
		return nil, fmt.Errorf("store: unknown table %q", table)
	}
	var where []string
	var args []any
	// deterministic order of where clauses
	for _, c := range def.Indexes {
		if v, ok := q.Where[c]; ok {
			where = append(where, quoteIdent(c)+" = ?")
			args = append(args, v)
		}
	}
	for k := range q.Where {
		if k == "id" {
			where = append(where, "id = ?")
			args = append(args, q.Where["id"])
			continue
		}
		found := false
		for _, c := range def.Indexes {
			if c == k {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("store: %q is not an index column of %s", k, table)
		}
	}
	sqlq := fmt.Sprintf("SELECT %s FROM %s", selectCols(def), table)
	if len(where) > 0 {
		sqlq += " WHERE " + strings.Join(where, " AND ")
	}
	order := "id"
	if q.OrderBy == "updated_at" {
		order = "updated_at"
	}
	dir := "ASC"
	if q.Desc {
		dir = "DESC"
	}
	sqlq += fmt.Sprintf(" ORDER BY %s %s, id ASC", order, dir)
	if q.Limit > 0 {
		sqlq += fmt.Sprintf(" LIMIT %d", q.Limit)
	}
	rows, err := s.b.query(ctx, sqlq, args...)
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(rows))
	for _, row := range rows {
		r, err := rowToRecord(def, row)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *sqlStore) publish(c Change) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for w := range s.watchers[c.Table] {
		select {
		case w.ch <- c:
		default: // slow consumer: drop (documented)
		}
	}
}

func (s *sqlStore) Watch(ctx context.Context, table string) (<-chan Change, error) {
	if _, ok := tableDef(table); !ok {
		return nil, fmt.Errorf("store: unknown table %q", table)
	}
	w := &watcher{ch: make(chan Change, 256)}
	s.mu.Lock()
	if s.watchers[table] == nil {
		s.watchers[table] = map[*watcher]struct{}{}
	}
	s.watchers[table][w] = struct{}{}
	s.mu.Unlock()

	remote, err := s.b.subscribe(ctx, table)
	if err != nil {
		s.mu.Lock()
		delete(s.watchers[table], w)
		s.mu.Unlock()
		return nil, err
	}
	out := make(chan Change, 256)
	go func() {
		defer close(out)
		defer func() {
			s.mu.Lock()
			delete(s.watchers[table], w)
			s.mu.Unlock()
		}()
		for {
			var c Change
			select {
			case <-ctx.Done():
				return
			case c = <-w.ch:
			case rc, ok := <-remote: // nil channel blocks forever if unsupported
				if !ok {
					remote = nil
					continue
				}
				c = rc
			}
			select {
			case out <- c:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func toString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	default:
		return 0
	}
}

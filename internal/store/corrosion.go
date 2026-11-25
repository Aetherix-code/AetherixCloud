package store

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CorrosionConfig configures the Corrosion sidecar connection.
type CorrosionConfig struct {
	// API is the base URL of Corrosion's HTTP API, e.g. "http://127.0.0.1:8080".
	API string
	// HTTPClient optionally overrides the client (tests).
	HTTPClient *http.Client
}

// OpenCorrosion returns a Store backed by a Corrosion agent.
//
// Corrosion owns the schema: it loads *.sql files from its schema directory
// at start-up. Write them with WriteCorrosionSchema before starting the
// sidecar. Migrate here only verifies that the tables exist.
func OpenCorrosion(cfg CorrosionConfig) Store {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	b := &corrosionBackend{base: strings.TrimRight(cfg.API, "/"), hc: hc}
	s := newSQLStore(b)
	s.migrate = func(ctx context.Context) error {
		for _, t := range Tables {
			if _, err := b.query(ctx, fmt.Sprintf("SELECT id FROM %s LIMIT 1", t.Name)); err != nil {
				return fmt.Errorf("corrosion schema missing table %s (write the schema dir and reload): %w", t.Name, err)
			}
		}
		return nil
	}
	return s
}

// WriteCorrosionSchema writes the schema file(s) into dir (Corrosion's
// `schema_paths`).
func WriteCorrosionSchema(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "aetherix.sql"), []byte(SchemaSQL()), 0o644)
}

type corrosionBackend struct {
	base string
	hc   *http.Client
}

// corrosion statement JSON (corro-api-types::Statement, untagged):
//   string                         -> Simple
//   ["SQL", [params...]]           -> WithParams
// Empty/nil args must NOT serialize as ["SQL", null] — that fails deserialize.
func corroStmt(s stmt) any {
	if len(s.args) == 0 {
		return s.sql
	}
	return []any{s.sql, s.args}
}

func (b *corrosionBackend) exec(ctx context.Context, stmts []stmt) error {
	if len(stmts) == 0 {
		return nil
	}
	body := make([]any, len(stmts))
	for i, s := range stmts {
		body[i] = corroStmt(s)
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, b.base+"/v1/transactions", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("corrosion transactions: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var out struct {
		Results []struct {
			Error string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &out); err == nil {
		for _, r := range out.Results {
			if r.Error != "" {
				return fmt.Errorf("corrosion: %s", r.Error)
			}
		}
	}
	return nil
}

type corroEvent struct {
	Columns []string        `json:"columns"`
	Row     json.RawMessage `json:"row"`
	EOQ     json.RawMessage `json:"eoq"`
	Change  json.RawMessage `json:"change"`
	Error   string          `json:"error"`
}

func (b *corrosionBackend) query(ctx context.Context, q string, args ...any) ([][]any, error) {
	buf, _ := json.Marshal(corroStmt(stmt{q, args}))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, b.base+"/v1/queries", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("corrosion queries: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var rows [][]any
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev corroEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, err
		}
		switch {
		case ev.Error != "":
			return nil, fmt.Errorf("corrosion: %s", ev.Error)
		case ev.Row != nil:
			vals, err := decodeCorroRow(ev.Row)
			if err != nil {
				return nil, err
			}
			rows = append(rows, vals)
		}
	}
	return rows, sc.Err()
}

// decodeCorroRow decodes [rowid, [v1, v2, ...]].
func decodeCorroRow(raw json.RawMessage) ([]any, error) {
	var pair []json.RawMessage
	if err := json.Unmarshal(raw, &pair); err != nil || len(pair) != 2 {
		return nil, fmt.Errorf("corrosion: malformed row %s", raw)
	}
	var vals []any
	dec := json.NewDecoder(bytes.NewReader(pair[1]))
	dec.UseNumber()
	if err := dec.Decode(&vals); err != nil {
		return nil, err
	}
	for i, v := range vals {
		if n, ok := v.(json.Number); ok {
			if iv, err := n.Int64(); err == nil {
				vals[i] = iv
			} else if fv, err := n.Float64(); err == nil {
				vals[i] = fv
			}
		}
	}
	return vals, nil
}

// subscribe opens a Corrosion subscription on the table and converts change
// events to Changes. Rows come back as (id, data, updated_at).
func (b *corrosionBackend) subscribe(ctx context.Context, table string) (<-chan Change, error) {
	def, ok := tableDef(table)
	if !ok {
		return nil, fmt.Errorf("unknown table %s", table)
	}
	q := fmt.Sprintf("SELECT %s FROM %s", selectCols(def), table)
	buf, _ := json.Marshal(corroStmt(stmt{q, nil}))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, b.base+"/v1/subscriptions?skip_rows=true", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	hc := *b.hc
	hc.Timeout = 0 // long-lived stream
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		return nil, fmt.Errorf("corrosion subscriptions: %s", resp.Status)
	}
	out := make(chan Change, 256)
	go func() {
		defer resp.Body.Close()
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			var ev corroEvent
			if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Change == nil {
				continue
			}
			// change: ["insert"|"update"|"delete", rowid, [values], change_id]
			var parts []json.RawMessage
			if json.Unmarshal(ev.Change, &parts) != nil || len(parts) < 3 {
				continue
			}
			var kind string
			_ = json.Unmarshal(parts[0], &kind)
			dec := json.NewDecoder(bytes.NewReader(parts[2]))
			dec.UseNumber()
			var vals []any
			if dec.Decode(&vals) != nil {
				continue
			}
			for i, v := range vals {
				if n, ok := v.(json.Number); ok {
					if iv, err := n.Int64(); err == nil {
						vals[i] = iv
					}
				}
			}
			rec, err := rowToRecord(def, vals)
			if err != nil {
				continue
			}
			c := Change{Table: table, ID: rec.ID, Op: OpPut, Record: rec}
			if kind == "delete" {
				c.Op, c.Record = OpDelete, Record{}
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

func (b *corrosionBackend) close() error { return nil }

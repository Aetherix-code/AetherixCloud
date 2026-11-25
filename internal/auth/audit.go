package auth

import (
	"context"
	"strings"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/ids"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// WriteAudit appends an entry to audit_log (best effort; errors are returned
// but callers should not fail the operation because of them).
func WriteAudit(ctx context.Context, s store.Store, e *v1.AuditEntry) error {
	if e.Id == "" {
		e.Id = ids.New()
	}
	if e.Time == nil {
		e.Time = timestamppb.Now()
	}
	return store.PutProto(ctx, s, store.TableAuditLog, e.Id, map[string]string{"actor": e.ActorUserId}, e)
}

// ListAudit returns entries newest first, optionally filtered.
func ListAudit(ctx context.Context, s store.Store, actor, actionPrefix string, limit int) ([]*v1.AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q := store.Query{OrderBy: "id", Desc: true}
	if actor != "" {
		q.Where = map[string]string{"actor": actor}
	}
	if actionPrefix == "" {
		q.Limit = limit
	}
	all, err := store.ListProto[v1.AuditEntry](ctx, s, store.TableAuditLog, q)
	if err != nil {
		return nil, err
	}
	out := make([]*v1.AuditEntry, 0, min(limit, len(all)))
	for _, e := range all {
		if strings.HasPrefix(e.Action, actionPrefix) {
			out = append(out, e)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

// PruneAudit deletes entries older than maxAge.
func PruneAudit(ctx context.Context, s store.Store, maxAge time.Duration) (int, error) {
	recs, err := s.List(ctx, store.TableAuditLog, store.Query{OrderBy: "updated_at"})
	if err != nil {
		return 0, err
	}
	cut := time.Now().Add(-maxAge).UnixNano()
	n := 0
	for _, r := range recs {
		if r.UpdatedAt < cut {
			if s.Delete(ctx, store.TableAuditLog, r.ID) == nil {
				n++
			}
		}
	}
	return n, nil
}

// sensitive field names never copied into audit attributes.
var sensitive = []string{"pass", "secret", "token", "value", "key", "yaml", "plaintext", "env"}

// AttrsOf extracts short, non-sensitive scalar fields of a request message
// (e.g. service_id, name, replicas) for the audit trail.
func AttrsOf(m proto.Message) map[string]string {
	out := map[string]string{}
	if m == nil {
		return out
	}
	r := m.ProtoReflect()
	fields := r.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fd.IsList() || fd.IsMap() || fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind || !r.Has(fd) {
			continue
		}
		name := string(fd.Name())
		skip := false
		for _, s := range sensitive {
			if strings.Contains(name, s) {
				skip = true
			}
		}
		if skip {
			continue
		}
		v := r.Get(fd).String()
		if fd.Kind() == protoreflect.StringKind && len(v) > 120 {
			v = v[:120] + "…"
		}
		out[name] = v
	}
	return out
}

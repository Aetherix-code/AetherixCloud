package store

import (
	"context"
	"encoding/json"
	"errors"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// PutProto stores a protobuf message as protojson.
func PutProto(ctx context.Context, s Store, table, id string, idx map[string]string, m proto.Message) error {
	data, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	if err != nil {
		return err
	}
	return s.Put(ctx, table, Record{ID: id, Idx: idx, Data: data})
}

// UnmarshalProto decodes a record into m.
func UnmarshalProto(r Record, m proto.Message) error {
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(r.Data, m)
}

// GetProto loads a protobuf message; returns ErrNotFound if absent.
func GetProto[T any, P interface {
	*T
	proto.Message
}](ctx context.Context, s Store, table, id string) (P, error) {
	r, err := s.Get(ctx, table, id)
	if err != nil {
		return nil, err
	}
	m := P(new(T))
	if err := UnmarshalProto(r, m); err != nil {
		return nil, err
	}
	return m, nil
}

// ListProto loads all messages matching q.
func ListProto[T any, P interface {
	*T
	proto.Message
}](ctx context.Context, s Store, table string, q Query) ([]P, error) {
	recs, err := s.List(ctx, table, q)
	if err != nil {
		return nil, err
	}
	out := make([]P, 0, len(recs))
	for _, r := range recs {
		m := P(new(T))
		if err := UnmarshalProto(r, m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// PutJSON stores an arbitrary JSON-serialisable value (internal rows).
func PutJSON(ctx context.Context, s Store, table, id string, idx map[string]string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.Put(ctx, table, Record{ID: id, Idx: idx, Data: data})
}

// GetJSON loads a JSON value.
func GetJSON[T any](ctx context.Context, s Store, table, id string) (*T, error) {
	r, err := s.Get(ctx, table, id)
	if err != nil {
		return nil, err
	}
	v := new(T)
	if err := json.Unmarshal(r.Data, v); err != nil {
		return nil, err
	}
	return v, nil
}

// ListJSON loads all JSON rows matching q.
func ListJSON[T any](ctx context.Context, s Store, table string, q Query) ([]T, error) {
	recs, err := s.List(ctx, table, q)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(recs))
	for _, r := range recs {
		var v T
		if err := json.Unmarshal(r.Data, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// IsNotFound reports whether err is ErrNotFound.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

package ingress

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/aetherix/aetherixcloud/internal/store"
)

// Ingress state lives in store.TableSettings under the id prefixes below.
const (
	prefixDrain   = "ingress/drain/"
	prefixApplied = "ingress/applied/"
	prefixCaddy   = "caddy/" // certmagic storage
)

func putSetting(ctx context.Context, st store.Store, id string, v any) error {
	return store.PutJSON(ctx, st, store.TableSettings, id, nil, v)
}

// listPrefix returns all settings rows whose id starts with prefix.
func listPrefix(ctx context.Context, st store.Store, prefix string) ([]store.Record, error) {
	recs, err := st.List(ctx, store.TableSettings, store.Query{})
	if err != nil {
		return nil, err
	}
	var out []store.Record
	for _, r := range recs {
		if strings.HasPrefix(r.ID, prefix) {
			out = append(out, r)
		}
	}
	return out, nil
}

func decode[T any](r store.Record) (T, error) {
	var v T
	err := json.Unmarshal(r.Data, &v)
	return v, err
}

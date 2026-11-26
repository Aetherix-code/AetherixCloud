package build

import (
	"context"

	"github.com/aetherix/aetherixcloud/internal/store"
)

const concurrencySettingID = "build/concurrency"

// ConcurrencyConfig is the cluster-wide build parallelism setting.
// Builds beyond the limit stay queued until a slot frees.
type ConcurrencyConfig struct {
	// MaxParallel is how many builds may run at once on each builder node.
	// 0 → default (2). Cluster-wide queueing is automatic: excess stay queued.
	MaxParallel int `json:"max_parallel"`
}

const DefaultMaxParallel = 2

// GetConcurrencyConfig loads the parallelism setting.
func GetConcurrencyConfig(ctx context.Context, st store.Store) (ConcurrencyConfig, error) {
	c, err := store.GetJSON[ConcurrencyConfig](ctx, st, store.TableSettings, concurrencySettingID)
	if err != nil {
		if store.IsNotFound(err) {
			return ConcurrencyConfig{MaxParallel: DefaultMaxParallel}, nil
		}
		return ConcurrencyConfig{}, err
	}
	if c.MaxParallel <= 0 {
		c.MaxParallel = DefaultMaxParallel
	}
	return *c, nil
}

// PutConcurrencyConfig saves the parallelism setting.
func PutConcurrencyConfig(ctx context.Context, st store.Store, c ConcurrencyConfig) error {
	if c.MaxParallel <= 0 {
		c.MaxParallel = DefaultMaxParallel
	}
	if c.MaxParallel > 64 {
		c.MaxParallel = 64
	}
	return store.PutJSON(ctx, st, store.TableSettings, concurrencySettingID, nil, c)
}

// EffectiveMaxParallel returns the live per-builder limit (always ≥ 1).
func EffectiveMaxParallel(ctx context.Context, st store.Store) int {
	c, err := GetConcurrencyConfig(ctx, st)
	if err != nil || c.MaxParallel <= 0 {
		return DefaultMaxParallel
	}
	return c.MaxParallel
}

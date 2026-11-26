package build

import (
	"context"
	"testing"

	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
)

func TestConcurrencyConfigDefaultAndClamp(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()

	c, err := GetConcurrencyConfig(ctx, st)
	require.NoError(t, err)
	require.Equal(t, DefaultMaxParallel, c.MaxParallel)
	require.Equal(t, DefaultMaxParallel, EffectiveMaxParallel(ctx, st))

	require.NoError(t, PutConcurrencyConfig(ctx, st, ConcurrencyConfig{MaxParallel: 0}))
	c, err = GetConcurrencyConfig(ctx, st)
	require.NoError(t, err)
	require.Equal(t, DefaultMaxParallel, c.MaxParallel)

	require.NoError(t, PutConcurrencyConfig(ctx, st, ConcurrencyConfig{MaxParallel: 8}))
	require.Equal(t, 8, EffectiveMaxParallel(ctx, st))

	require.NoError(t, PutConcurrencyConfig(ctx, st, ConcurrencyConfig{MaxParallel: 999}))
	c, err = GetConcurrencyConfig(ctx, st)
	require.NoError(t, err)
	require.Equal(t, 64, c.MaxParallel)
}

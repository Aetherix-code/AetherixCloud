package cron_test

import (
	"testing"
	"time"

	"github.com/aetherix/aetherixcloud/internal/cron"
	"github.com/stretchr/testify/require"
)

func TestMatch(t *testing.T) {
	now := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC) // Mon
	ok, err := cron.Match("0 3 * * *", now)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = cron.Match("0 4 * * *", now)
	require.NoError(t, err)
	require.False(t, ok)

	ok, err = cron.Match("*/15 * * * *", time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC))
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = cron.Match("0 3 * * 2", now) // Tuesday 2026-11-24
	require.NoError(t, err)
	require.True(t, ok)

	_, err = cron.Match("bad", now)
	require.Error(t, err)
}

package backup_test

import (
	"testing"
	"time"

	"github.com/aetherix/aetherixcloud/internal/backup"
	"github.com/stretchr/testify/require"
)

func TestMatchCron(t *testing.T) {
	now := time.Date(2026, 6, 15, 3, 0, 0, 0, time.UTC)
	ok, err := backup.MatchCron("0 3 * * *", now)
	require.NoError(t, err)
	require.True(t, ok)
}

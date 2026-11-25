package backup

import (
	"time"

	"github.com/aetherix/aetherixcloud/internal/cron"
)

// MatchCron evaluates a 5-field cron expression against t (UTC).
func MatchCron(expr string, t time.Time) (bool, error) {
	return cron.Match(expr, t)
}

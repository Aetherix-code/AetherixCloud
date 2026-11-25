package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Match evaluates a 5-field cron expression (min hour dom month dow)
// against t. Supports "*", "n", "*/n", "a-b", and comma lists. Dow: 0=Sunday.
func Match(expr string, t time.Time) (bool, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return false, fmt.Errorf("cron: want 5 fields, got %d", len(fields))
	}
	checks := []struct {
		field string
		v, min, max int
	}{
		{fields[0], t.Minute(), 0, 59},
		{fields[1], t.Hour(), 0, 23},
		{fields[2], t.Day(), 1, 31},
		{fields[3], int(t.Month()), 1, 12},
		{fields[4], int(t.Weekday()), 0, 6},
	}
	for _, c := range checks {
		ok, err := matchField(c.field, c.v, c.min, c.max)
		if err != nil || !ok {
			return ok, err
		}
	}
	return true, nil
}

func matchField(field string, v, min, max int) (bool, error) {
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "*" {
			return true, nil
		}
		if strings.HasPrefix(part, "*/") {
			step, err := strconv.Atoi(part[2:])
			if err != nil || step <= 0 {
				return false, fmt.Errorf("cron step %q", part)
			}
			if (v-min)%step == 0 {
				return true, nil
			}
			continue
		}
		if a, b, ok := strings.Cut(part, "-"); ok {
			lo, err1 := strconv.Atoi(a)
			hi, err2 := strconv.Atoi(b)
			if err1 != nil || err2 != nil {
				return false, fmt.Errorf("cron range %q", part)
			}
			if v >= lo && v <= hi {
				return true, nil
			}
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return false, fmt.Errorf("cron value %q", part)
		}
		if n < min || n > max {
			return false, fmt.Errorf("cron value %d out of range [%d,%d]", n, min, max)
		}
		if v == n {
			return true, nil
		}
	}
	return false, nil
}

package upgrade

import (
	"context"
	"time"

	"github.com/aetherix/aetherixcloud/internal/store"
)

const settingID = "upgrade"

// Config is the cluster-wide desired upgrade state (replicated via settings).
type Config struct {
	Channel        string    `json:"channel,omitempty"`         // stable | latest
	DesiredVersion string    `json:"desired_version,omitempty"` // e.g. v0.4.0; empty = no pending target
	DownloadBase   string    `json:"download_base,omitempty"`
	Repo           string    `json:"repo,omitempty"` // owner/name; default aetherix/aetherixcloud
	MachineIDs     []string  `json:"machine_ids,omitempty"` // empty = all
	StartedAt      time.Time `json:"started_at,omitempty"`
	StartedBy      string    `json:"started_by,omitempty"`
}

// GetConfig loads upgrade settings.
func GetConfig(ctx context.Context, st store.Store) (Config, error) {
	c, err := store.GetJSON[Config](ctx, st, store.TableSettings, settingID)
	if err != nil {
		if store.IsNotFound(err) {
			return Config{Channel: "latest"}, nil
		}
		return Config{}, err
	}
	if c.Channel == "" {
		c.Channel = "latest"
	}
	return *c, nil
}

// PutConfig saves upgrade settings.
func PutConfig(ctx context.Context, st store.Store, c Config) error {
	if c.Channel == "" {
		c.Channel = "latest"
	}
	return store.PutJSON(ctx, st, store.TableSettings, settingID, nil, c)
}

// Targets reports whether machineID should apply the desired version.
func (c Config) Targets(machineID string) bool {
	if c.DesiredVersion == "" {
		return false
	}
	if len(c.MachineIDs) == 0 {
		return true
	}
	for _, id := range c.MachineIDs {
		if id == machineID {
			return true
		}
	}
	return false
}

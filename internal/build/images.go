package build

import (
	"context"

	"github.com/aetherix/aetherixcloud/internal/store"
)

const imageSettingID = "build/images"

// ImageArchiveConfig is the cluster-wide setting for archiving build images
// to S3 and pruning older local tags (saves disk on builder nodes).
type ImageArchiveConfig struct {
	Enabled  bool   `json:"enabled"`
	TargetID string `json:"target_id"`
	// Retain is how many successful image archives to keep per service in S3
	// (and roughly how many recent local tags to keep). 0 → default 5.
	Retain int `json:"retain"`
}

// GetImageArchiveConfig loads cluster image-archive settings.
func GetImageArchiveConfig(ctx context.Context, st store.Store) (ImageArchiveConfig, error) {
	c, err := store.GetJSON[ImageArchiveConfig](ctx, st, store.TableSettings, imageSettingID)
	if err != nil {
		if store.IsNotFound(err) {
			return ImageArchiveConfig{Retain: 5}, nil
		}
		return ImageArchiveConfig{}, err
	}
	if c.Retain <= 0 {
		c.Retain = 5
	}
	return *c, nil
}

// PutImageArchiveConfig saves cluster image-archive settings.
func PutImageArchiveConfig(ctx context.Context, st store.Store, c ImageArchiveConfig) error {
	if c.Retain <= 0 {
		c.Retain = 5
	}
	return store.PutJSON(ctx, st, store.TableSettings, imageSettingID, nil, c)
}

// DefaultImageRetain is used when retain is unset.
const DefaultImageRetain = 5

// ResolveImageArchive picks retain + target from the global setting, with
// optional per-service BuildSpec overrides (legacy).
func ResolveImageArchive(ctx context.Context, st store.Store, retainOverride int, targetOverride string) (enabled bool, retain int, targetID string, err error) {
	cfg, err := GetImageArchiveConfig(ctx, st)
	if err != nil {
		return false, 0, "", err
	}
	if !cfg.Enabled && retainOverride <= 0 {
		return false, 0, "", nil
	}
	retain = cfg.Retain
	targetID = cfg.TargetID
	if retainOverride > 0 {
		retain = retainOverride
	}
	if targetOverride != "" {
		targetID = targetOverride
	}
	if retain <= 0 {
		retain = DefaultImageRetain
	}
	return true, retain, targetID, nil
}

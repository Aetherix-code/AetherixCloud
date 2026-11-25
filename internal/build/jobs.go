package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/ids"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Build statuses (v1.Build.status).
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// JobMeta is the source description of a queued build. It is stored in the
// settings table ("buildjob/<build id>") next to the builds row because
// v1.Build has no room for it.
type JobMeta struct {
	BuildID   string `json:"build_id"`
	ServiceID string `json:"service_id"`
	// Kind is "github" (clone through the GitHub App) or "upload".
	Kind           string `json:"kind"`
	Repo           string `json:"repo,omitempty"`
	InstallationID int64  `json:"installation_id,omitempty"`
	// Ref is "refs/heads/<branch>" or "refs/pull/<n>/head".
	Ref      string `json:"ref,omitempty"`
	SHA      string `json:"sha,omitempty"`
	PRNumber int    `json:"pr_number,omitempty"`
	Message  string `json:"message,omitempty"`
	Author   string `json:"author,omitempty"`
	// Delivery is the X-GitHub-Delivery that caused the build ("" for simulated pushes).
	Delivery string `json:"delivery,omitempty"`
}

const jobPrefix = "buildjob/"

// DeterministicBuildID derives a stable build ID from a webhook delivery and
// the service, so two nodes processing the same delivery collapse onto one row.
func DeterministicBuildID(delivery, serviceID string) string {
	h := sha256.Sum256([]byte(delivery + "/" + serviceID))
	return "b" + strings.ToLower(hex.EncodeToString(h[:12]))
}

// PutBuild stores a build row with consistent index columns.
func PutBuild(ctx context.Context, st store.Store, b *v1.Build) error {
	return store.PutProto(ctx, st, store.TableBuilds, b.Id, map[string]string{"service_id": b.ServiceId, "status": b.Status}, b)
}

// GetBuild loads a build.
func GetBuild(ctx context.Context, st store.Store, id string) (*v1.Build, error) {
	return store.GetProto[v1.Build](ctx, st, store.TableBuilds, id)
}

// LoadJob loads the job metadata of a build.
func LoadJob(ctx context.Context, st store.Store, buildID string) (*JobMeta, error) {
	return store.GetJSON[JobMeta](ctx, st, store.TableSettings, jobPrefix+buildID)
}

// Enqueue creates a queued build for meta. When meta.BuildID is set and the
// build already exists it is returned unchanged (created == false).
func Enqueue(ctx context.Context, st store.Store, meta JobMeta) (b *v1.Build, created bool, err error) {
	if meta.BuildID == "" {
		meta.BuildID = ids.New()
	}
	if ex, err := GetBuild(ctx, st, meta.BuildID); err == nil {
		return ex, false, nil
	}
	if err := store.PutJSON(ctx, st, store.TableSettings, jobPrefix+meta.BuildID, nil, meta); err != nil {
		return nil, false, err
	}
	b = &v1.Build{Id: meta.BuildID, ServiceId: meta.ServiceID, CommitSha: meta.SHA, Ref: meta.Ref,
		Status: StatusQueued, CreatedAt: timestamppb.Now()}
	if err := PutBuild(ctx, st, b); err != nil {
		return nil, false, err
	}
	_ = store.PutEvent(ctx, st, &v1.Event{Id: ids.New(), Type: "build.queued", SubjectId: b.Id,
		Message: "build queued", Time: timestamppb.Now(), Attributes: map[string]string{"service_id": b.ServiceId}})
	return b, true, nil
}

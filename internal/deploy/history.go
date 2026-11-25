package deploy

import (
	"context"
	"sort"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
)

// HistoryEntry is a deployment with its lifecycle events (oldest first).
type HistoryEntry struct {
	Deployment *v1.Deployment
	Events     []*v1.Event
}

// History returns the newest-first deployment history of a service with the
// events recorded for each deployment (deployment.created/deploying/active/
// rolled_back/failed).
func History(ctx context.Context, st store.Store, serviceID string, limit int) ([]HistoryEntry, error) {
	deps, err := store.ListDeployments(ctx, st, serviceID, limit)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(deps, func(i, j int) bool { return deps[i].CreatedAt.AsTime().After(deps[j].CreatedAt.AsTime()) })
	evs, err := store.ListProto[v1.Event](ctx, st, store.TableEvents, store.Query{})
	if err != nil {
		return nil, err
	}
	by := map[string][]*v1.Event{}
	for _, e := range evs {
		by[e.SubjectId] = append(by[e.SubjectId], e)
	}
	out := make([]HistoryEntry, 0, len(deps))
	for _, d := range deps {
		es := by[d.Id]
		sort.SliceStable(es, func(i, j int) bool { return es[i].Time.AsTime().Before(es[j].Time.AsTime()) })
		out = append(out, HistoryEntry{Deployment: d, Events: es})
	}
	return out, nil
}

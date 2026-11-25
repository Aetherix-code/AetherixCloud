// Package events is the cluster activity stream: append, list and live-watch
// helpers over store.TableEvents. Watch is fed by store subscriptions
// (store.Watch), so on a Corrosion cluster events written on any node reach
// every node's subscribers; ClusterService.WatchEvents is a thin wrapper.
package events

import (
	"context"
	"strings"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/ids"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Emit appends an event.
func Emit(ctx context.Context, s store.Store, typ, subject, msg string, attrs map[string]string) (*v1.Event, error) {
	e := &v1.Event{Id: ids.New(), Type: typ, SubjectId: subject, Message: msg, Attributes: attrs, Time: timestamppb.Now()}
	return e, store.PutEvent(ctx, s, e)
}

// List returns up to limit events, newest first, whose type has the prefix.
func List(ctx context.Context, s store.Store, prefix string, limit int) ([]*v1.Event, error) {
	if limit <= 0 {
		limit = 100
	}
	// Over-fetch when filtering so a rare prefix still fills the page.
	fetch := limit
	if prefix != "" {
		fetch = limit * 4
	}
	evs, err := store.ListProto[v1.Event](ctx, s, store.TableEvents, store.Query{OrderBy: "updated_at", Desc: true, Limit: fetch})
	if err != nil {
		return nil, err
	}
	out := make([]*v1.Event, 0, limit)
	for _, e := range evs {
		if strings.HasPrefix(e.Type, prefix) {
			out = append(out, e)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

// Watch replays the last `replay` matching events (oldest first) and then
// streams new ones until ctx is done, when the channel is closed. Slow
// consumers block the producer goroutine, not the store.
func Watch(ctx context.Context, s store.Store, prefix string, replay int) (<-chan *v1.Event, error) {
	// Subscribe before replaying so nothing is lost between the two.
	ch, err := s.Watch(ctx, store.TableEvents)
	if err != nil {
		return nil, err
	}
	var past []*v1.Event
	if replay > 0 {
		if past, err = List(ctx, s, prefix, replay); err != nil {
			return nil, err
		}
	}
	out := make(chan *v1.Event, 64)
	go func() {
		defer close(out)
		seen := map[string]bool{}
		send := func(e *v1.Event) bool {
			select {
			case out <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for i := len(past) - 1; i >= 0; i-- {
			seen[past[i].Id] = true
			if !send(past[i]) {
				return
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case c, ok := <-ch:
				if !ok {
					return
				}
				if c.Op != store.OpPut {
					continue
				}
				e := &v1.Event{}
				if store.UnmarshalProto(c.Record, e) != nil || !strings.HasPrefix(e.Type, prefix) || seen[e.Id] {
					continue
				}
				delete(seen, e.Id)
				if !send(e) {
					return
				}
			}
		}
	}()
	return out, nil
}

// Prune deletes events older than maxAge and anything beyond maxCount newest.
func Prune(ctx context.Context, s store.Store, maxAge time.Duration, maxCount int) (int, error) {
	recs, err := s.List(ctx, store.TableEvents, store.Query{OrderBy: "updated_at", Desc: true})
	if err != nil {
		return 0, err
	}
	cut := time.Now().Add(-maxAge).UnixNano()
	n := 0
	for i, r := range recs {
		if (i >= maxCount || r.UpdatedAt < cut) && s.Delete(ctx, store.TableEvents, r.ID) == nil {
			n++
		}
	}
	return n, nil
}

// Run prunes events (7 days / 10k rows) and, via pruneAudit, the audit log
// (90 days) every hour until ctx is done. Safe to run on every node.
func Run(ctx context.Context, s store.Store, pruneAudit func(context.Context) error) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		_, _ = Prune(ctx, s, 7*24*time.Hour, 10000)
		if pruneAudit != nil {
			_ = pruneAudit(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

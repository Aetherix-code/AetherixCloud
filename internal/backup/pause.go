package backup

import (
	"context"
	"fmt"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const pauseWait = 2 * time.Minute

// pauseService scales a replicated service to 0 and waits until replicas are
// stopped so a volume restore can run safely. Returns the previous replica
// count (for resume). Global services and missing services are left alone
// (prevReplicas 0, no error for missing — caller may still restore).
func pauseService(ctx context.Context, st store.Store, serviceID string) (prevReplicas int32, err error) {
	svc, err := store.GetService(ctx, st, serviceID)
	if err != nil {
		if store.IsNotFound(err) {
			return 0, nil
		}
		return 0, err
	}
	if svc.Spec == nil {
		return 0, nil
	}
	if svc.Spec.Mode == v1.ServiceMode_SERVICE_MODE_GLOBAL {
		return 0, fmt.Errorf("cannot pause global service %s for volume restore; scale it down manually first", svc.Name)
	}
	prevReplicas = svc.Spec.Replicas
	if prevReplicas == 0 {
		return 0, waitReplicasQuiesced(ctx, st, serviceID)
	}
	if err := setServiceReplicas(ctx, st, svc, 0); err != nil {
		return prevReplicas, err
	}
	if err := waitReplicasQuiesced(ctx, st, serviceID); err != nil {
		return prevReplicas, err
	}
	return prevReplicas, nil
}

func resumeService(ctx context.Context, st store.Store, serviceID string, prevReplicas int32) error {
	if prevReplicas <= 0 {
		return nil
	}
	svc, err := store.GetService(ctx, st, serviceID)
	if err != nil {
		return err
	}
	if svc.Spec == nil {
		return nil
	}
	return setServiceReplicas(ctx, st, svc, prevReplicas)
}

func setServiceReplicas(ctx context.Context, st store.Store, svc *v1.Service, n int32) error {
	svc.Spec.Replicas = n
	svc.UpdatedAt = timestamppb.Now()
	if err := store.PutService(ctx, st, svc); err != nil {
		return err
	}
	if svc.ActiveDeploymentId != "" {
		if d, err := store.GetDeployment(ctx, st, svc.ActiveDeploymentId); err == nil && d.Spec != nil {
			d.Spec.Replicas = n
			_ = store.PutDeployment(ctx, st, d)
		}
	}
	return nil
}

func waitReplicasQuiesced(ctx context.Context, st store.Store, serviceID string) error {
	deadline := time.Now().Add(pauseWait)
	for {
		reps, err := store.ListReplicas(ctx, st, map[string]string{"service_id": serviceID})
		if err != nil {
			return err
		}
		busy := 0
		for _, r := range reps {
			if r.DesiredRunning {
				busy++
				continue
			}
			switch r.State {
			case v1.ReplicaState_REPLICA_STATE_STOPPED, v1.ReplicaState_REPLICA_STATE_UNSPECIFIED:
				// ok
			default:
				// Still draining / stopping / running.
				if r.State != v1.ReplicaState_REPLICA_STATE_STOPPED {
					busy++
				}
			}
		}
		if busy == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for service %s replicas to stop", serviceID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

package registry

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/runtime"
	"github.com/aetherix/aetherixcloud/internal/store"
)

// PeerPuller pulls "127.0.0.1:5000/..." images from peer registries.
type PeerPuller struct {
	Store  store.Store
	Self   string // own machine ID (skipped)
	Logger *slog.Logger
}

// Pull implements reconciler.Puller.
func (p PeerPuller) Pull(ctx context.Context, rt runtime.Runtime, ref string) error {
	if !strings.HasPrefix(ref, LocalHost+"/") {
		return rt.PullImage(ctx, ref)
	}
	if err := rt.PullImage(ctx, ref); err == nil {
		return nil // we have it locally
	}
	machines, err := store.ListMachines(ctx, p.Store)
	if err != nil {
		return err
	}
	rest := strings.TrimPrefix(ref, LocalHost+"/")
	var lastErr error = fmt.Errorf("no peer has %s", ref)
	for _, m := range machines {
		if m.Id == p.Self || m.State == v1.MachineState_MACHINE_STATE_DOWN || m.MeshIp == "" {
			continue
		}
		peerRef := fmt.Sprintf("%s:%d/%s", m.MeshIp, Port, rest)
		if err := rt.PullImage(ctx, peerRef); err != nil {
			lastErr = err
			continue
		}
		if err := rt.TagImage(ctx, peerRef, ref); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("pull %s from peers: %w", ref, lastErr)
}

package api

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
)

// TCP proxy public ports are allocated in this range so they stay clear of
// privileged / well-known ports and typical app container ports.
const (
	tcpProxyPortMin = 20000
	tcpProxyPortMax = 49999
)

// AssignTCPPorts fills PublishedPort for every mode=tcp port that has none,
// picking a cluster-wide unique random port shared by all ingress nodes.
func AssignTCPPorts(ctx context.Context, st store.Store, spec *v1.ServiceSpec, exceptServiceID string) error {
	if spec == nil {
		return nil
	}
	used, err := usedPublishedTCPPorts(ctx, st, exceptServiceID)
	if err != nil {
		return err
	}
	for _, p := range spec.Ports {
		if p == nil || p.Mode != "tcp" {
			continue
		}
		if p.PublishedPort > 0 {
			used[int(p.PublishedPort)] = true
			continue
		}
		port, err := pickFreeTCPPort(used)
		if err != nil {
			return err
		}
		p.PublishedPort = int32(port)
		used[port] = true
	}
	return nil
}

func usedPublishedTCPPorts(ctx context.Context, st store.Store, exceptServiceID string) (map[int]bool, error) {
	svcs, err := store.ListServices(ctx, st, nil)
	if err != nil {
		return nil, err
	}
	used := map[int]bool{}
	for _, svc := range svcs {
		if svc.Id == exceptServiceID {
			continue
		}
		for _, p := range svc.GetSpec().GetPorts() {
			if p.GetMode() == "tcp" && p.GetPublishedPort() > 0 {
				used[int(p.PublishedPort)] = true
			}
		}
	}
	return used, nil
}

// preserveTCPPublished copies published ports from old → new for matching
// tcp container ports so compose redeploys keep a stable ingress port.
func preserveTCPPublished(old, neu *v1.ServiceSpec) {
	if old == nil || neu == nil {
		return
	}
	byContainer := map[int32]int32{}
	for _, p := range old.Ports {
		if p.GetMode() == "tcp" && p.GetPublishedPort() > 0 {
			byContainer[p.ContainerPort] = p.PublishedPort
		}
	}
	for _, p := range neu.Ports {
		if p.GetMode() != "tcp" || p.GetPublishedPort() > 0 {
			continue
		}
		if pub, ok := byContainer[p.ContainerPort]; ok {
			p.PublishedPort = pub
		}
	}
}

func pickFreeTCPPort(used map[int]bool) (int, error) {
	span := tcpProxyPortMax - tcpProxyPortMin + 1
	var buf [8]byte
	for attempt := 0; attempt < 64; attempt++ {
		if _, err := rand.Read(buf[:]); err != nil {
			return 0, err
		}
		n := int(binary.BigEndian.Uint64(buf[:])%uint64(span)) + tcpProxyPortMin
		if !used[n] {
			return n, nil
		}
	}
	// Linear scan fallback.
	for n := tcpProxyPortMin; n <= tcpProxyPortMax; n++ {
		if !used[n] {
			return n, nil
		}
	}
	return 0, fmt.Errorf("no free TCP proxy ports in %d–%d", tcpProxyPortMin, tcpProxyPortMax)
}

//go:build !linux

package mesh

import (
	"context"
	"fmt"
	"net/netip"
	"runtime"
)

type systemLink struct{}

func newSystemLink() (Link, error) {
	return nil, fmt.Errorf("mesh: WireGuard device management is only supported on Linux (running on %s); use `ax dev` to run nodes in containers", runtime.GOOS)
}

func (systemLink) Ensure(context.Context, string, netip.Prefix, int) (string, error) {
	return "", fmt.Errorf("unsupported OS")
}
func (systemLink) Delete(string) error { return nil }

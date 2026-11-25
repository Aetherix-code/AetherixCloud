//go:build linux

package mesh

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"time"

	"github.com/vishvananda/netlink"
)

type systemLink struct{}

func newSystemLink() (Link, error) { return systemLink{}, nil }

// Ensure tries the in-kernel WireGuard first and falls back to wireguard-go
// (userspace, needs /dev/net/tun) when the kernel lacks the module.
func (systemLink) Ensure(ctx context.Context, name string, addr netip.Prefix, mtu int) (string, error) {
	impl := "kernel"
	if l, err := netlink.LinkByName(name); err == nil {
		if l.Type() != "wireguard" {
			impl = "userspace"
		}
	} else {
		attrs := netlink.NewLinkAttrs()
		attrs.Name = name
		if err := netlink.LinkAdd(&netlink.GenericLink{LinkAttrs: attrs, LinkType: "wireguard"}); err != nil {
			// kernel module unavailable: userspace fallback
			if err2 := startWireguardGo(ctx, name); err2 != nil {
				return "", fmt.Errorf("kernel wireguard: %v; wireguard-go: %w", err, err2)
			}
			impl = "userspace"
		}
	}
	l, err := netlink.LinkByName(name)
	if err != nil {
		return "", err
	}
	nlAddr, err := netlink.ParseAddr(addr.String())
	if err != nil {
		return "", err
	}
	if err := netlink.AddrReplace(l, nlAddr); err != nil {
		return "", fmt.Errorf("assign %s: %w", addr, err)
	}
	if err := netlink.LinkSetMTU(l, mtu); err != nil {
		return "", err
	}
	if err := netlink.LinkSetUp(l); err != nil {
		return "", err
	}
	enableForwarding()
	return impl, nil
}

func startWireguardGo(ctx context.Context, name string) error {
	bin, err := exec.LookPath("wireguard-go")
	if err != nil {
		return fmt.Errorf("wireguard-go not found in PATH")
	}
	// wireguard-go daemonises itself unless -f is given.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), bin, name)
	cmd.Env = append(os.Environ(), "WG_PROCESS_FOREGROUND=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("wireguard-go: %v: %s", err, out)
	}
	for i := 0; i < 50; i++ {
		if _, err := netlink.LinkByName(name); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("wireguard-go did not create %s", name)
}

func (systemLink) Delete(name string) error {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return nil
	}
	return netlink.LinkDel(l)
}

// enableForwarding turns on IPv4 forwarding and keeps overlay traffic
// un-NATed (best effort; needs root).
func enableForwarding() {
	_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644)
	if ipt, err := exec.LookPath("iptables"); err == nil {
		net := NetworkCIDR
		for _, args := range [][]string{
			{"-t", "nat", "-C", "POSTROUTING", "-s", net, "-d", net, "-j", "RETURN"},
		} {
			if exec.Command(ipt, args...).Run() != nil {
				_ = exec.Command(ipt, "-t", "nat", "-I", "POSTROUTING", "1", "-s", net, "-d", net, "-j", "RETURN").Run()
			}
		}
		if exec.Command(ipt, "-C", "FORWARD", "-s", net, "-d", net, "-j", "ACCEPT").Run() != nil {
			_ = exec.Command(ipt, "-I", "FORWARD", "1", "-s", net, "-d", net, "-j", "ACCEPT").Run()
		}
	}
}

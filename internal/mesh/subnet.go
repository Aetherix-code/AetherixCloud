// Package mesh manages the WireGuard overlay: key generation, subnet
// allocation, device configuration (kernel WireGuard via netlink, or the
// wireguard-go userspace fallback) and endpoint fallback for NATed peers.
//
// Addressing (cluster network 10.210.0.0/16):
//
//	machine subnet   10.210.N.0/24    (N = 1..254, allocated at join time)
//	wg interface     10.210.N.1/16    "machine mesh IP"; also hosts DNS (:53)
//	docker bridge    10.210.N.0/24    gateway 10.210.N.2, containers 10.210.N.128/25
//
// A peer's AllowedIPs is its machine /24 (covers its mesh IP and containers).
package mesh

import (
	"fmt"
	"net/netip"
)

// NetworkCIDR is the default cluster network.
const NetworkCIDR = "10.210.0.0/16"

// Network returns the cluster network prefix.
func Network() netip.Prefix { return netip.MustParsePrefix(NetworkCIDR) }

// AllocateSubnet returns the lowest free /24 inside network (default
// Network()) that is not in used. Subnet .0 is reserved.
func AllocateSubnet(network netip.Prefix, used []netip.Prefix) (netip.Prefix, error) {
	if !network.IsValid() {
		network = Network()
	}
	if network.Bits() != 16 {
		return netip.Prefix{}, fmt.Errorf("mesh: network %s must be a /16", network)
	}
	taken := map[netip.Prefix]bool{}
	for _, u := range used {
		taken[netip.PrefixFrom(u.Addr(), 24).Masked()] = true
	}
	base := network.Masked().Addr().As4()
	for n := 1; n <= 254; n++ {
		p := netip.PrefixFrom(netip.AddrFrom4([4]byte{base[0], base[1], byte(n), 0}), 24)
		if !taken[p] {
			return p, nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("mesh: no free /24 left in %s", network)
}

func hostAddr(subnet netip.Prefix, host byte) netip.Addr {
	b := subnet.Masked().Addr().As4()
	b[3] = host
	return netip.AddrFrom4(b)
}

// MeshIP is the machine's address on the overlay (subnet .1).
func MeshIP(subnet netip.Prefix) netip.Addr { return hostAddr(subnet, 1) }

// BridgeGateway is the docker bridge gateway for the subnet (.2).
func BridgeGateway(subnet netip.Prefix) netip.Addr { return hostAddr(subnet, 2) }

// ContainerRange is the docker IP range for containers (.128/25).
func ContainerRange(subnet netip.Prefix) netip.Prefix {
	return netip.PrefixFrom(hostAddr(subnet, 128), 25)
}

// InterfaceAddress is the wg interface address: the mesh IP with the cluster
// network's prefix length so the whole network routes through wg.
func InterfaceAddress(subnet netip.Prefix, network netip.Prefix) netip.Prefix {
	if !network.IsValid() {
		network = Network()
	}
	return netip.PrefixFrom(MeshIP(subnet), network.Bits())
}

package daemon

import "net/netip"

type netipPrefix = netip.Prefix

func parsePrefix(s string) (netip.Prefix, error) { return netip.ParsePrefix(s) }

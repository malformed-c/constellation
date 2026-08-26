// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package manager

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/vishvananda/netlink"

	"github.com/cilium/cilium/pkg/datapath/linux/safenetlink"
)

const (
	logOverrides = "overrides"
	logOriginal  = "original"
	logOverride  = "override"
	logReachable = "reachable"
	logCandidate = "candidate"
	logFallback  = "fallback"
)

// parseTunnelEndpointOverrides parses a comma-separated "node=ip" string into
// a map keyed by remote node name. Returns nil for an empty input.
func parseTunnelEndpointOverrides(raw string) (map[string]netip.Addr, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	m := make(map[string]netip.Addr)
	for entry := range strings.SplitSeq(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid tunnel-endpoint-override entry %q: expected node=ip", entry)
		}
		name := strings.TrimSpace(parts[0])
		ipStr := strings.TrimSpace(parts[1])
		if name == "" || ipStr == "" {
			return nil, fmt.Errorf("invalid tunnel-endpoint-override entry %q: expected node=ip", entry)
		}
		addr, err := netip.ParseAddr(ipStr)
		if err != nil {
			return nil, fmt.Errorf("invalid IP in tunnel-endpoint-override %q: %w", entry, err)
		}
		m[name] = addr
	}
	return m, nil
}

// lookupTunnelEndpointOverride returns the override tunnel endpoint for a
// remote node if (a) an override is configured for nodeName AND (b) the
// override IP is within a locally-connected subnet. If either condition
// fails, fallback is returned unchanged. The reachability guard makes
// the override safe to set cluster-wide: nodes without L2 adjacency to
// the override IP silently ignore it.
func lookupTunnelEndpointOverride(overrides map[string]netip.Addr, nodeName string, fallback netip.Addr) netip.Addr {
	if overrides == nil {
		return fallback
	}
	addr, ok := overrides[nodeName]
	if !ok {
		return fallback
	}
	if !isLocallyReachable(addr) {
		return fallback
	}
	return addr
}

// connectedPrefixesFunc is the function used to obtain connected prefixes.
// Overridable in tests.
var connectedPrefixesFunc = connectedPrefixes

// isLocallyReachable returns true if addr falls within any prefix
// directly assigned to a local network interface.
func isLocallyReachable(addr netip.Addr) bool {
	for _, prefix := range connectedPrefixesFunc() {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// connectedPrefixes returns the IPv4/IPv6 prefixes assigned to local
// network interfaces (e.g. 192.168.50.2/24 → 192.168.50.0/24).
func connectedPrefixes() []netip.Prefix {
	// Not net.Interfaces()/iface.Addrs(): those query the kernel over a
	// netlink socket with no timeout and can block forever (cilium#15051),
	// which is why the linter forbids them. safenetlink bounds the call.
	links, err := safenetlink.LinkList()
	if err != nil {
		return nil
	}
	var prefixes []netip.Prefix
	for _, link := range links {
		addrs, err := safenetlink.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if a.IPNet == nil {
				continue
			}
			addr, ok := netip.AddrFromSlice(a.IPNet.IP)
			if !ok {
				continue
			}
			ones, _ := a.IPNet.Mask.Size()
			prefixes = append(prefixes, netip.PrefixFrom(addr.Unmap(), ones).Masked())
		}
	}
	return prefixes
}

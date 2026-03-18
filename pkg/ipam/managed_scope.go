// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Constellation

package ipam

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"sync"
	"time"

	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	nodeTypes "github.com/cilium/cilium/pkg/node/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cilium/cilium/pkg/ip"
	"github.com/cilium/cilium/pkg/ipam/service/ipallocator"
	"github.com/cilium/cilium/pkg/k8s/client"
	"github.com/cilium/cilium/pkg/logging/logfields"
)

// managedScopeAllocator extends ClusterPool IPAM to support multiple managed
// nodes (perigeos pawns) sharing a single agent. Each managed node's
// CiliumNode provides a /24 CIDR; this allocator merges them into one pool
// and distributes allocations round-robin across sub-allocators.
type managedScopeAllocator struct {
	logger *slog.Logger

	mu        sync.Mutex
	subs      []*subAllocator // ordered list of sub-allocators
	subByNode map[string]*subAllocator
	nextIdx   int // round-robin index
}

type subAllocator struct {
	nodeName  string
	allocCIDR *net.IPNet
	allocator *ipallocator.Range
}

// newManagedScopeAllocator creates a managed allocator by fetching CiliumNodes
// for all managed names and building a sub-allocator per CIDR.
func newManagedScopeAllocator(
	ctx context.Context,
	logger *slog.Logger,
	cs client.Clientset,
	primaryCIDR *net.IPNet,
) *managedScopeAllocator {
	m := &managedScopeAllocator{
		logger:    logger,
		subByNode: make(map[string]*subAllocator),
	}

	// Always include the primary node's CIDR.
	localName := nodeTypes.GetName()
	m.addCIDR(localName, primaryCIDR)

	// Fetch CiliumNodes for all managed names (except the primary, already added).
	managedNames := nodeTypes.GetManagedNames()
	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	for _, name := range managedNames {
		if name == localName {
			continue
		}

		cn, err := cs.CiliumV2().CiliumNodes().Get(fetchCtx, name, metav1.GetOptions{})
		if err != nil {
			logger.Warn("Managed IPAM: could not fetch CiliumNode, skipping",
				logfields.NodeName, name,
				logfields.Error, err,
			)
			continue
		}

		cidr := ciliumNodeIPv4CIDR(cn)
		if cidr == nil {
			logger.Warn("Managed IPAM: CiliumNode has no IPv4 podCIDR, skipping",
				logfields.NodeName, name)
			continue
		}

		m.addCIDR(name, cidr)
	}

	logger.Info("Managed IPAM: initialized",
		logfields.Nodes, len(m.subs),
		logfields.Capacity, m.Capacity(),
	)

	return m
}

func (m *managedScopeAllocator) addCIDR(nodeName string, cidr *net.IPNet) {
	sub := &subAllocator{
		nodeName:  nodeName,
		allocCIDR: cidr,
		allocator: ipallocator.NewCIDRRange(cidr),
	}
	m.subs = append(m.subs, sub)
	m.subByNode[nodeName] = sub
	m.logger.Info("Managed IPAM: added sub-allocator",
		logfields.NodeName, nodeName,
		logfields.V4Prefix, cidr.String())
}

// findSubForIP returns the sub-allocator whose CIDR contains ip.
func (m *managedScopeAllocator) findSubForIP(ipAddr net.IP) *subAllocator {
	for _, sub := range m.subs {
		if sub.allocCIDR.Contains(ipAddr) {
			return sub
		}
	}
	return nil
}

// Allocate allocates a specific IP. Routes to the sub-allocator owning the CIDR.
func (m *managedScopeAllocator) Allocate(ipAddr net.IP, owner string, pool Pool) (*AllocationResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	sub := m.findSubForIP(ipAddr)
	if sub == nil {
		return nil, fmt.Errorf("IP %s does not belong to any managed CIDR", ipAddr)
	}

	if err := sub.allocator.Allocate(ipAddr); err != nil {
		return nil, err
	}
	return &AllocationResult{IP: ipAddr}, nil
}

// AllocateWithoutSyncUpstream is identical to Allocate for host-scope.
func (m *managedScopeAllocator) AllocateWithoutSyncUpstream(ipAddr net.IP, owner string, pool Pool) (*AllocationResult, error) {
	return m.Allocate(ipAddr, owner, pool)
}

// Release releases an IP back to the sub-allocator owning the CIDR.
func (m *managedScopeAllocator) Release(ipAddr net.IP, pool Pool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	sub := m.findSubForIP(ipAddr)
	if sub == nil {
		return fmt.Errorf("IP %s does not belong to any managed CIDR", ipAddr)
	}

	sub.allocator.Release(ipAddr)
	return nil
}

// AllocateNext allocates the next available IP using round-robin across
// sub-allocators. If a sub-allocator is full, the next one is tried.
func (m *managedScopeAllocator) AllocateNext(owner string, pool Pool) (*AllocationResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	n := len(m.subs)
	for range n {
		sub := m.subs[m.nextIdx%n]
		m.nextIdx++

		ipAddr, err := sub.allocator.AllocateNext()
		if err != nil {
			// This sub-allocator is full, try the next one.
			continue
		}

		return &AllocationResult{IP: ipAddr}, nil
	}

	return nil, fmt.Errorf("all managed CIDR pools exhausted (%d pools)", n)
}

// AllocateNextWithoutSyncUpstream is identical to AllocateNext for host-scope.
func (m *managedScopeAllocator) AllocateNextWithoutSyncUpstream(owner string, pool Pool) (*AllocationResult, error) {
	return m.AllocateNext(owner, pool)
}

// Dump returns a merged dump of all sub-allocators.
func (m *managedScopeAllocator) Dump() (map[Pool]map[string]string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	alloc := map[string]string{}
	var totalCapacity uint64

	for _, sub := range m.subs {
		_, data, err := sub.allocator.Snapshot()
		if err != nil {
			continue
		}

		var origIP *big.Int
		if sub.allocCIDR.IP.To4() != nil {
			origIP = big.NewInt(0).SetBytes(sub.allocCIDR.IP.To4())
		} else {
			origIP = big.NewInt(0).SetBytes(sub.allocCIDR.IP.To16())
		}
		bits := big.NewInt(0).SetBytes(data)
		for i := range bits.BitLen() {
			if bits.Bit(i) != 0 {
				allocated := net.IP(big.NewInt(0).Add(origIP, big.NewInt(int64(uint(i+1)))).Bytes()).String()
				alloc[allocated] = ""
			}
		}
		totalCapacity += ip.CountIPsInCIDR(sub.allocCIDR).Uint64()
	}

	status := fmt.Sprintf("%d/%d allocated from %d managed CIDRs", len(alloc), totalCapacity, len(m.subs))
	return map[Pool]map[string]string{PoolDefault(): alloc}, status
}

// Capacity returns the total capacity across all sub-allocators.
func (m *managedScopeAllocator) Capacity() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	var total uint64
	for _, sub := range m.subs {
		total += ip.CountIPsInCIDR(sub.allocCIDR).Uint64()
	}
	return total
}

// RestoreFinished is a no-op for host-scope allocators.
func (m *managedScopeAllocator) RestoreFinished() {}

// ciliumNodeIPv4CIDR extracts the first IPv4 podCIDR from a CiliumNode spec.
func ciliumNodeIPv4CIDR(cn *ciliumv2.CiliumNode) *net.IPNet {
	for _, cidrStr := range cn.Spec.IPAM.PodCIDRs {
		_, cidr, err := net.ParseCIDR(cidrStr)
		if err != nil {
			continue
		}
		if cidr.IP.To4() != nil {
			return cidr
		}
	}
	return nil
}

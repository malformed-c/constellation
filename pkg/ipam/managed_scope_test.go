// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Constellation

package ipam

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
)

var testLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func parseCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, cidr, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("parseCIDR(%q): %v", s, err)
	}
	return cidr
}

func newTestManagedAllocator(t *testing.T, cidrs ...string) *managedScopeAllocator {
	t.Helper()
	m := &managedScopeAllocator{
		logger:    testLogger,
		subByNode: make(map[string]*subAllocator),
	}
	for i, cidrStr := range cidrs {
		name := fmt.Sprintf("node-%02d", i)
		m.addCIDR(name, parseCIDR(t, cidrStr))
	}
	return m
}

func TestManagedScope_AllocateNext_RoundRobin(t *testing.T) {
	m := newTestManagedAllocator(t, "10.0.0.0/30", "10.0.1.0/30")
	// /30 = 4 addresses, 2 usable (network + broadcast excluded by ipallocator)

	seen := map[string]bool{}
	for i := range 4 {
		result, err := m.AllocateNext(fmt.Sprintf("owner-%d", i), PoolDefault())
		if err != nil {
			t.Fatalf("AllocateNext %d: %v", i, err)
		}
		seen[result.IP.String()] = true
	}

	// Should have IPs from both CIDRs
	has0 := false
	has1 := false
	for ip := range seen {
		parsed := net.ParseIP(ip)
		if parseCIDR(t, "10.0.0.0/30").Contains(parsed) {
			has0 = true
		}
		if parseCIDR(t, "10.0.1.0/30").Contains(parsed) {
			has1 = true
		}
	}
	if !has0 || !has1 {
		t.Errorf("expected IPs from both CIDRs, got: %v", seen)
	}
}

func TestManagedScope_AllocateNext_Exhaustion(t *testing.T) {
	m := newTestManagedAllocator(t, "10.0.0.0/30", "10.0.1.0/30")

	// Allocate all available IPs
	var count int
	for {
		_, err := m.AllocateNext(fmt.Sprintf("owner-%d", count), PoolDefault())
		if err != nil {
			break
		}
		count++
		if count > 100 {
			t.Fatal("too many allocations, expected exhaustion")
		}
	}

	if count == 0 {
		t.Fatal("expected at least one allocation")
	}
}

func TestManagedScope_AllocateAndRelease(t *testing.T) {
	m := newTestManagedAllocator(t, "10.0.0.0/24")

	ip := net.ParseIP("10.0.0.5")
	result, err := m.Allocate(ip, "test-owner", PoolDefault())
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if !result.IP.Equal(ip) {
		t.Errorf("got %s, want %s", result.IP, ip)
	}

	// Double allocate should fail
	_, err = m.Allocate(ip, "other-owner", PoolDefault())
	if err == nil {
		t.Error("expected error on double allocate")
	}

	// Release and re-allocate should work
	if err := m.Release(ip, PoolDefault()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	_, err = m.Allocate(ip, "new-owner", PoolDefault())
	if err != nil {
		t.Fatalf("re-Allocate after release: %v", err)
	}
}

func TestManagedScope_AllocateOutOfRange(t *testing.T) {
	m := newTestManagedAllocator(t, "10.0.0.0/24")

	_, err := m.Allocate(net.ParseIP("192.168.0.1"), "owner", PoolDefault())
	if err == nil {
		t.Error("expected error for IP outside managed CIDRs")
	}
}

func TestManagedScope_ReleaseOutOfRange(t *testing.T) {
	m := newTestManagedAllocator(t, "10.0.0.0/24")

	err := m.Release(net.ParseIP("192.168.0.1"), PoolDefault())
	if err == nil {
		t.Error("expected error for releasing IP outside managed CIDRs")
	}
}

func TestManagedScope_Capacity(t *testing.T) {
	m := newTestManagedAllocator(t, "10.0.0.0/24", "10.0.1.0/24")

	// Capacity reports raw CIDR size (ipallocator may exclude net/broadcast).
	// ip.CountIPsInCIDR counts all addresses in the range.
	cap := m.Capacity()
	if cap < 500 {
		t.Errorf("Capacity() = %d, expected at least 500 for two /24s", cap)
	}
}

func TestManagedScope_Dump(t *testing.T) {
	m := newTestManagedAllocator(t, "10.0.0.0/24")

	_, err := m.AllocateNext("owner-1", PoolDefault())
	if err != nil {
		t.Fatalf("AllocateNext: %v", err)
	}

	dump, status := m.Dump()
	if len(dump[PoolDefault()]) != 1 {
		t.Errorf("expected 1 allocated IP in dump, got %d", len(dump[PoolDefault()]))
	}
	if status == "" {
		t.Error("expected non-empty status")
	}
}

func TestManagedScope_SkipsFullAllocator(t *testing.T) {
	// Use /30 (4 addrs, 2 usable) and /24 (256 addrs, 254 usable).
	// Exhaust the /30 by allocating specific IPs, then verify AllocateNext
	// still works by falling through to the /24.
	m := newTestManagedAllocator(t, "10.0.0.0/30", "10.0.1.0/24")

	cidr30 := parseCIDR(t, "10.0.0.0/30")

	// Exhaust the /30 pool by allocating specific IPs (10.0.0.1 and 10.0.0.2).
	for _, ipStr := range []string{"10.0.0.1", "10.0.0.2"} {
		_, err := m.Allocate(net.ParseIP(ipStr), "fill-"+ipStr, PoolDefault())
		if err != nil {
			t.Fatalf("Allocate(%s): %v", ipStr, err)
		}
	}

	// Now AllocateNext should skip the full /30 and return IPs from the /24.
	for i := range 5 {
		result, err := m.AllocateNext(fmt.Sprintf("next-%d", i), PoolDefault())
		if err != nil {
			t.Fatalf("AllocateNext %d after /30 exhaustion: %v", i, err)
		}
		if cidr30.Contains(result.IP) {
			t.Errorf("AllocateNext %d returned %s from exhausted /30", i, result.IP)
		}
	}
}

func TestManagedScope_MultipleNodes(t *testing.T) {
	m := &managedScopeAllocator{
		logger:    testLogger,
		subByNode: make(map[string]*subAllocator),
	}
	m.addCIDR("primary", parseCIDR(t, "10.0.0.0/24"))
	m.addCIDR("pawn-01", parseCIDR(t, "10.0.1.0/24"))
	m.addCIDR("pawn-02", parseCIDR(t, "10.0.2.0/24"))

	if len(m.subs) != 3 {
		t.Fatalf("expected 3 sub-allocators, got %d", len(m.subs))
	}
	if m.subByNode["pawn-01"] == nil {
		t.Error("subByNode missing pawn-01")
	}
	if m.subByNode["pawn-02"] == nil {
		t.Error("subByNode missing pawn-02")
	}

	// Capacity should be ~3 * 254 (network/broadcast excluded)
	if m.Capacity() < 750 {
		t.Errorf("Capacity() = %d, expected at least 750 for three /24s", m.Capacity())
	}
}

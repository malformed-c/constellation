// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Constellation

package ipam

import (
	"fmt"
	"log/slog"
	"net"
	"testing"
)

var testLogger = slog.New(slog.DiscardHandler)

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
	// Set primary to the first node.
	if len(cidrs) > 0 {
		m.primaryNode = "node-00"
	}
	return m
}

func TestManagedScope_AllocateNext_PoolRouting(t *testing.T) {
	m := &managedScopeAllocator{
		logger:      testLogger,
		subByNode:   make(map[string]*subAllocator),
		primaryNode: "primary",
	}
	m.addCIDR("primary", parseCIDR(t, "10.0.0.0/24"))
	m.addCIDR("pawn-01", parseCIDR(t, "10.0.1.0/24"))
	m.addCIDR("pawn-02", parseCIDR(t, "10.0.2.0/24"))

	// Allocate from pawn-01's pool
	r1, err := m.AllocateNext("owner-1", Pool("pawn-01"))
	if err != nil {
		t.Fatalf("AllocateNext pawn-01: %v", err)
	}
	if !parseCIDR(t, "10.0.1.0/24").Contains(r1.IP) {
		t.Errorf("expected IP from 10.0.1.0/24, got %s", r1.IP)
	}
	if r1.IPPoolName != Pool("pawn-01") {
		t.Errorf("expected IPPoolName=pawn-01, got %s", r1.IPPoolName)
	}

	// Allocate from pawn-02's pool
	r2, err := m.AllocateNext("owner-2", Pool("pawn-02"))
	if err != nil {
		t.Fatalf("AllocateNext pawn-02: %v", err)
	}
	if !parseCIDR(t, "10.0.2.0/24").Contains(r2.IP) {
		t.Errorf("expected IP from 10.0.2.0/24, got %s", r2.IP)
	}
	if r2.IPPoolName != Pool("pawn-02") {
		t.Errorf("expected IPPoolName=pawn-02, got %s", r2.IPPoolName)
	}

	// Empty pool falls back to primary
	r3, err := m.AllocateNext("owner-3", Pool(""))
	if err != nil {
		t.Fatalf("AllocateNext empty pool: %v", err)
	}
	if !parseCIDR(t, "10.0.0.0/24").Contains(r3.IP) {
		t.Errorf("expected IP from primary 10.0.0.0/24, got %s", r3.IP)
	}
	if r3.IPPoolName != Pool("primary") {
		t.Errorf("expected IPPoolName=primary, got %s", r3.IPPoolName)
	}
}

func TestManagedScope_AllocateNext_UnknownPoolFallback(t *testing.T) {
	m := &managedScopeAllocator{
		logger:      testLogger,
		subByNode:   make(map[string]*subAllocator),
		primaryNode: "primary",
	}
	m.addCIDR("primary", parseCIDR(t, "10.0.0.0/24"))

	// Unknown pool name should fall back to primary
	result, err := m.AllocateNext("owner", Pool("nonexistent-pawn"))
	if err != nil {
		t.Fatalf("AllocateNext unknown pool: %v", err)
	}
	if !parseCIDR(t, "10.0.0.0/24").Contains(result.IP) {
		t.Errorf("expected fallback to primary CIDR, got %s", result.IP)
	}
}

func TestManagedScope_AllocateNext_Exhaustion(t *testing.T) {
	m := newTestManagedAllocator(t, "10.0.0.0/30")

	// Allocate all available IPs from node-00 (the only pool, also primary)
	var count int
	for {
		_, err := m.AllocateNext(fmt.Sprintf("owner-%d", count), Pool("node-00"))
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
	if result.IPPoolName != Pool("node-00") {
		t.Errorf("expected IPPoolName=node-00, got %s", result.IPPoolName)
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

	_, err := m.AllocateNext("owner-1", Pool("node-00"))
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

func TestManagedScope_AddCIDR_Dynamic(t *testing.T) {
	m := &managedScopeAllocator{
		logger:      testLogger,
		subByNode:   make(map[string]*subAllocator),
		primaryNode: "primary",
	}
	m.addCIDR("primary", parseCIDR(t, "10.0.0.0/24"))

	// Dynamically add a new node's CIDR.
	m.AddCIDR("pawn-new", parseCIDR(t, "10.0.5.0/24"))

	// Allocate from the dynamically added pool.
	result, err := m.AllocateNext("owner", Pool("pawn-new"))
	if err != nil {
		t.Fatalf("AllocateNext from dynamic pool: %v", err)
	}
	if !parseCIDR(t, "10.0.5.0/24").Contains(result.IP) {
		t.Errorf("expected IP from 10.0.5.0/24, got %s", result.IP)
	}

	// Adding the same node with the same CIDR is a no-op.
	m.AddCIDR("pawn-new", parseCIDR(t, "10.0.5.0/24"))
	if len(m.subs) != 2 {
		t.Errorf("expected 2 sub-allocators after same-CIDR AddCIDR, got %d", len(m.subs))
	}

	// Adding the same node with a DIFFERENT CIDR replaces the sub-allocator.
	m.AddCIDR("pawn-new", parseCIDR(t, "10.0.6.0/24"))
	if len(m.subs) != 2 {
		t.Errorf("expected 2 sub-allocators after CIDR update, got %d", len(m.subs))
	}

	// Allocate from the updated pool — should come from the new CIDR.
	result2, err := m.AllocateNext("owner2", Pool("pawn-new"))
	if err != nil {
		t.Fatalf("AllocateNext from updated pool: %v", err)
	}
	if !parseCIDR(t, "10.0.6.0/24").Contains(result2.IP) {
		t.Errorf("expected IP from updated 10.0.6.0/24, got %s", result2.IP)
	}

	// Old CIDR should no longer be allocatable via pool routing.
	if m.subByNode["pawn-new"].allocCIDR.String() != "10.0.6.0/24" {
		t.Errorf("expected sub-allocator CIDR to be 10.0.6.0/24, got %s",
			m.subByNode["pawn-new"].allocCIDR.String())
	}
}

func TestManagedScope_AddCIDR_UpdateExisting(t *testing.T) {
	m := &managedScopeAllocator{
		logger:      testLogger,
		subByNode:   make(map[string]*subAllocator),
		primaryNode: "primary",
	}
	m.addCIDR("primary", parseCIDR(t, "10.0.0.0/24"))
	m.addCIDR("pawn-01", parseCIDR(t, "10.0.1.0/24"))

	// Allocate an IP from pawn-01's original CIDR.
	r1, err := m.AllocateNext("owner-1", Pool("pawn-01"))
	if err != nil {
		t.Fatalf("AllocateNext: %v", err)
	}
	if !parseCIDR(t, "10.0.1.0/24").Contains(r1.IP) {
		t.Errorf("expected IP from 10.0.1.0/24, got %s", r1.IP)
	}

	// Simulate CiliumNode recreation with a new CIDR.
	m.AddCIDR("pawn-01", parseCIDR(t, "10.0.5.0/24"))

	// Sub-allocator count should stay the same.
	if len(m.subs) != 2 {
		t.Fatalf("expected 2 sub-allocators after CIDR update, got %d", len(m.subs))
	}

	// New allocations should come from the updated CIDR.
	r2, err := m.AllocateNext("owner-2", Pool("pawn-01"))
	if err != nil {
		t.Fatalf("AllocateNext after update: %v", err)
	}
	if !parseCIDR(t, "10.0.5.0/24").Contains(r2.IP) {
		t.Errorf("expected IP from updated 10.0.5.0/24, got %s", r2.IP)
	}

	// Old IP should fail to release (old CIDR no longer tracked).
	err = m.Release(r1.IP, PoolDefault())
	if err == nil {
		t.Error("expected error releasing IP from old CIDR")
	}

	// Primary should be unaffected.
	r3, err := m.AllocateNext("owner-3", Pool(""))
	if err != nil {
		t.Fatalf("AllocateNext primary: %v", err)
	}
	if !parseCIDR(t, "10.0.0.0/24").Contains(r3.IP) {
		t.Errorf("expected IP from primary 10.0.0.0/24, got %s", r3.IP)
	}
}

func TestManagedScope_AddCIDR_SameCIDR_NoOp(t *testing.T) {
	m := &managedScopeAllocator{
		logger:      testLogger,
		subByNode:   make(map[string]*subAllocator),
		primaryNode: "primary",
	}
	m.addCIDR("primary", parseCIDR(t, "10.0.0.0/24"))
	m.addCIDR("pawn-01", parseCIDR(t, "10.0.1.0/24"))

	// Allocate an IP.
	r1, err := m.AllocateNext("owner", Pool("pawn-01"))
	if err != nil {
		t.Fatalf("AllocateNext: %v", err)
	}

	// AddCIDR with same CIDR should be a no-op — allocator state preserved.
	m.AddCIDR("pawn-01", parseCIDR(t, "10.0.1.0/24"))

	// The previously allocated IP should still be allocated (double-alloc fails).
	_, err = m.Allocate(r1.IP, "other", Pool("pawn-01"))
	if err == nil {
		t.Error("expected error on double allocate — same-CIDR AddCIDR must preserve state")
	}
}

func TestManagedScope_MultipleNodes(t *testing.T) {
	m := &managedScopeAllocator{
		logger:      testLogger,
		subByNode:   make(map[string]*subAllocator),
		primaryNode: "primary",
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

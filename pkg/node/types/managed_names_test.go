// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Constellation

package types

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// resetManagedNames clears the managed names state between tests.
// Tests that call SetManagedNames must defer this to avoid polluting siblings.
func resetManagedNames() {
	managedNodeNamesMu.Lock()
	managedNodeNames = nil
	managedNodeNamesMu.Unlock()
}

// TestGetManagedNames_DefaultsToNodeName verifies that when no managed names
// are set the function falls back to the single local node name — preserving
// standard Cilium behaviour.
func TestGetManagedNames_DefaultsToNodeName(t *testing.T) {
	defer resetManagedNames()

	SetName("host-01")
	names := GetManagedNames()
	require.Equal(t, []string{"host-01"}, names,
		"unset managed names should return [GetName()]")
}

// TestSetManagedNames_ReturnsConfiguredList verifies that after SetManagedNames
// the list is returned verbatim.
func TestSetManagedNames_ReturnsConfiguredList(t *testing.T) {
	defer resetManagedNames()

	pawns := []string{"pawn-0", "pawn-1", "pawn-2"}
	SetManagedNames(pawns)
	require.Equal(t, pawns, GetManagedNames())
}

// TestSetManagedNames_EmptySlice_FallsBackToNodeName verifies that setting an
// empty list restores the default fallback behaviour.
func TestSetManagedNames_EmptySlice_FallsBackToNodeName(t *testing.T) {
	defer resetManagedNames()

	SetName("host-02")
	SetManagedNames([]string{"pawn-0"})
	SetManagedNames([]string{}) // clear
	require.Equal(t, []string{"host-02"}, GetManagedNames())
}

// TestIsManaged_LocalNodeAlwaysManaged verifies that the physical host name is
// always managed regardless of what SetManagedNames was called with.
func TestIsManaged_LocalNodeAlwaysManaged(t *testing.T) {
	defer resetManagedNames()

	SetName("host-03")
	// No SetManagedNames — default fallback applies.
	require.True(t, IsManaged("host-03"))
}

// TestIsManaged_PawnNames verifies that pawn names passed to SetManagedNames
// are all reported as managed.
func TestIsManaged_PawnNames(t *testing.T) {
	defer resetManagedNames()

	SetManagedNames([]string{"pawn-0", "pawn-1", "pawn-2"})

	require.True(t, IsManaged("pawn-0"))
	require.True(t, IsManaged("pawn-1"))
	require.True(t, IsManaged("pawn-2"))
}

// TestIsManaged_UnknownName verifies that names not in the managed set return
// false — preventing endpoint restore from accepting foreign pods.
func TestIsManaged_UnknownName(t *testing.T) {
	defer resetManagedNames()

	SetManagedNames([]string{"pawn-0", "pawn-1"})

	require.False(t, IsManaged("pawn-2"),
		"pawn-2 not in managed set, should not be managed")
	require.False(t, IsManaged(""),
		"empty string should not be managed")
	require.False(t, IsManaged("pawn-0-evil"),
		"prefix match should not count as managed")
}

// TestIsManaged_CaseSensitive verifies that name matching is exact and
// case-sensitive, consistent with k8s node name semantics.
func TestIsManaged_CaseSensitive(t *testing.T) {
	defer resetManagedNames()

	SetManagedNames([]string{"Pawn-0"})
	require.False(t, IsManaged("pawn-0"),
		"managed name lookup must be case-sensitive")
}

// TestGetManagedNames_DoesNotReturnInternalSlice verifies that mutating the
// returned slice does not corrupt internal state.
func TestGetManagedNames_DoesNotReturnInternalSlice(t *testing.T) {
	defer resetManagedNames()

	SetManagedNames([]string{"pawn-0", "pawn-1"})
	names := GetManagedNames()
	names[0] = "tampered"

	// Internal state should be unaffected.
	require.True(t, IsManaged("pawn-0"),
		"mutating the returned slice should not affect internal state")
}

// TestManagedNames_PawnTopology is the canonical perigeos scenario:
// one physical host running two pawns, one constellation-agent managing both.
func TestManagedNames_PawnTopology(t *testing.T) {
	defer resetManagedNames()

	// Physical host name — this is what constellation-agent registers as.
	SetName("rack-07")

	// Perigeos calls SetManagedNames with all pawn names on this host.
	SetManagedNames([]string{"pawn-east-0", "pawn-east-1", "pawn-east-2"})

	// Pods scheduled on any pawn should be accepted.
	for _, pawn := range []string{"pawn-east-0", "pawn-east-1", "pawn-east-2"} {
		require.True(t, IsManaged(pawn), "pawn %q should be managed", pawn)
	}

	// A pod on a different host's pawn must be rejected.
	require.False(t, IsManaged("pawn-west-0"),
		"pod from a different host's pawn must not be accepted")

	// The physical hostname itself is not in the managed set here;
	// standard Cilium-style endpoints don't land on rack-07 directly.
	require.False(t, IsManaged("rack-07"),
		"physical host name is not in managed set when pawns are configured")
}

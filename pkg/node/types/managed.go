// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Constellation

package types

// Constellation fork addition: the set of k8s node names whose pods this
// agent manages (perigeos host sharding). Kept in its own file so that
// pkg/node/types/nodename.go remains byte-identical to upstream Cilium and
// never conflicts on rebase.

import (
	"slices"
	"sync"
)

var (
	// managedNodeNames is the set of k8s node names whose pods this agent
	// manages. In standard Cilium deployments this contains only nodeName.
	// In Constellation (perigeos host sharding) it is populated dynamically
	// by the node watcher from --managed-pawns-selector.
	managedNodeNames   []string
	managedNodeNamesMu sync.RWMutex
)

// SetManagedNames sets the list of k8s node names whose pods this agent
// manages. Called by the node watcher during bootstrap (initial list)
// and updated dynamically as nodes are added or removed.
// If names is empty, GetManagedNames defaults to [GetName()].
func SetManagedNames(names []string) {
	managedNodeNamesMu.Lock()
	defer managedNodeNamesMu.Unlock()
	managedNodeNames = names
}

// GetManagedNames returns a copy of the list of k8s node names this agent
// manages. Always returns at least one entry (the local node name).
// Returns a copy so callers cannot accidentally modify internal state.
func GetManagedNames() []string {
	managedNodeNamesMu.RLock()
	defer managedNodeNamesMu.RUnlock()
	if len(managedNodeNames) == 0 {
		return []string{nodeName}
	}
	return append([]string(nil), managedNodeNames...)
}

// IsManaged returns true if the given node name is managed by this agent.
func IsManaged(name string) bool {
	return slices.Contains(GetManagedNames(), name)
}

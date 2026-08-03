// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Constellation

package client

// Startup-time API server fall-through for the Constellation fork.
//
// Upstream already rotates through --k8s-api-server-urls when its own
// connection wait loop (waitForConn) cannot reach the current candidate. But
// that loop runs from a lifecycle start hook, and this fork does work during
// hive object-graph population that must reach the API server first — notably
// managed-node discovery in pkg/k8s/tables. Constructors run before start
// hooks, so that work would otherwise be pinned to whichever candidate was
// selected at init with no way to fall through to the next tier.
//
// Exposing rotation lets those callers walk the tier list themselves. This
// lives in its own file, and is reached through an optional interface rather
// than by widening the exported Clientset interface, so neither cell.go nor
// the test fake has to change — no upstream divergence to carry across a
// rebase.

// Guards the optional-interface lookup done by callers: if the clientset were
// ever wrapped or replaced, the type assertion on their side would silently
// stop matching and fall-through would quietly become a plain retry. Keep this
// in sync with pkg/k8s/tables' apiServerRotator.
var _ interface{ RotateAPIServerURL() bool } = (*compositeClientset)(nil)

// RotateAPIServerURL advances the client to the next configured API server
// candidate and reports whether it did.
//
// The candidate list is tier-ordered (each --k8s-api-server-urls value is one
// tier, tried in order; members within a tier are shuffled per-process), and
// rotation steps through it by index, so repeated calls walk the tiers in
// preference order and wrap around.
//
// Rotation takes effect immediately for clients already built from this
// clientset: the round tripper rewrites each request's host from the current
// candidate rather than baking it into the REST config.
//
// Returns false when rotation is not possible or not wanted — a single
// configured URL, or a client that has already graduated onto the kube-apiserver
// service address, where the datapath load balances across live backends and
// manual rotation would fight it.
func (c *compositeClientset) RotateAPIServerURL() bool {
	if c.restConfigManager == nil || !c.restConfigManager.canRotateAPIServerURL() {
		return false
	}
	c.restConfigManager.rotateAPIServerURL()
	return true
}

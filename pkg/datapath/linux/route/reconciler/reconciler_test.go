// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package reconciler

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestRouteReconcilerHandleConfigScopedToRouteOnly is a regression test for
// an agent-startup crash: an unscoped netlink handle also opens
// NETLINK_XFRM and NETLINK_NETFILTER sockets, which this reconciler never
// uses, and which made the whole agent fail to start (EPROTONOSUPPORT) on
// a host where the kernel's XFRM module was temporarily missing after an
// update - even though routing itself was completely unaffected. If this
// ever regresses back to requesting every family (or drops the explicit
// scoping entirely), this test catches it without needing a real kernel
// family gap to reproduce the crash.
func TestRouteReconcilerHandleConfigScopedToRouteOnly(t *testing.T) {
	require.NotNil(t, routeReconcilerHandleConfig)
	require.Equal(t, []int{unix.NETLINK_ROUTE}, routeReconcilerHandleConfig.NLFamilies,
		"route reconciler must request only NETLINK_ROUTE - it never uses XFRM or NETFILTER, "+
			"and requesting them makes startup depend on kernel features this reconciler doesn't need")
}

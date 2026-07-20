// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package linux

import (
	"net"
	"net/netip"
	"testing"

	"github.com/cilium/hive/hivetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"go4.org/netipx"
	"golang.org/x/sys/unix"

	"github.com/cilium/cilium/pkg/datapath/config"
	fakeipsec "github.com/cilium/cilium/pkg/datapath/linux/ipsec/fake"
	ipsecTypes "github.com/cilium/cilium/pkg/datapath/linux/ipsec/types"
	"github.com/cilium/cilium/pkg/datapath/linux/linux_defaults"
	"github.com/cilium/cilium/pkg/datapath/linux/route"
	"github.com/cilium/cilium/pkg/ip"
	"github.com/cilium/cilium/pkg/kpr"
	nodemapfake "github.com/cilium/cilium/pkg/maps/nodemap/fake"
	"github.com/cilium/cilium/pkg/mtu"
	"github.com/cilium/cilium/pkg/node"
	nodeaddressing "github.com/cilium/cilium/pkg/node/addressing"
	fakenode "github.com/cilium/cilium/pkg/node/fake"
	nodeTypes "github.com/cilium/cilium/pkg/node/types"
	"github.com/cilium/cilium/pkg/testutils"
	"github.com/cilium/cilium/pkg/testutils/netns"
)

var (
	fakeNodeAddressing = fakenode.NewAddressing()

	nodeConfig = config.Config{
		NodeIPv4:            ip.AddrFromIP(fakeNodeAddressing.IPv4().PrimaryExternal()),
		NodeIPv6:            ip.AddrFromIP(fakeNodeAddressing.IPv6().PrimaryExternal()),
		CiliumInternalIPv4:  ip.AddrFromIP(fakeNodeAddressing.IPv4().Router()),
		CiliumInternalIPv6:  ip.AddrFromIP(fakeNodeAddressing.IPv6().Router()),
		DeviceMTU:           calcMtu.DeviceMTU,
		RouteMTU:            calcMtu.RouteMTU,
		RoutePostEncryptMTU: calcMtu.RoutePostEncryptMTU,
	}
	mtuConfig = mtu.NewConfiguration(0, false, false, false, false)
	calcMtu   = mtuConfig.Calculate(100)
	nh        = linuxNodeHandler{
		nodeConfig: nodeConfig,
		datapathConfig: DatapathConfiguration{
			HostDevice: "host_device",
		},
	}
	cr1 = netip.MustParsePrefix("10.1.0.0/16")
)

func TestCreateNodeRoute(t *testing.T) {
	dpConfig := DatapathConfiguration{
		HostDevice: "host_device",
	}
	log := hivetest.Logger(t)

	lns := node.NewTestLocalNodeStore(node.LocalNode{})
	nodeHandler := newNodeHandler(log, dpConfig, nil, kpr.KPRConfig{}, &fakeipsec.Agent{}, fakeipsec.Config{}, lns)
	nodeHandler.NodeConfigurationChanged(nodeConfig)

	c1 := netip.MustParsePrefix("10.10.0.0/16")
	generatedRoute, err := nodeHandler.createNodeRouteSpec(c1, false)
	require.NoError(t, err)
	require.Equal(t, *netipx.PrefixIPNet(c1), generatedRoute.Prefix)
	require.Equal(t, dpConfig.HostDevice, generatedRoute.Device)
	require.Equal(t, fakeNodeAddressing.IPv4().Router().To4(), generatedRoute.Nexthop.To4())
	require.Equal(t, fakeNodeAddressing.IPv4().Router().To4(), generatedRoute.Local.To4())

	c1 = netip.MustParsePrefix("beef:beef::/48")
	generatedRoute, err = nodeHandler.createNodeRouteSpec(c1, false)
	require.NoError(t, err)
	require.Equal(t, *netipx.PrefixIPNet(c1), generatedRoute.Prefix)
	require.Equal(t, dpConfig.HostDevice, generatedRoute.Device)
	require.Nil(t, generatedRoute.Nexthop)
	require.Equal(t, fakeNodeAddressing.IPv6().Router().To16(), generatedRoute.Local.To16())
}

func TestCreateNodeRouteSpecMtu(t *testing.T) {
	generatedRoute, err := nh.createNodeRouteSpec(cr1, false)

	require.NoError(t, err)
	require.NotEqual(t, 0, generatedRoute.MTU)

	generatedRoute, err = nh.createNodeRouteSpec(cr1, true)

	require.NoError(t, err)
	require.Equal(t, 0, generatedRoute.MTU)
}

func TestPrivilegedLocalRule(t *testing.T) {
	testutils.PrivilegedTest(t)

	ns := netns.NewNetNS(t)

	test := func(t *testing.T) {
		require.NoError(t, NodeEnsureLocalRoutingRule())

		// Expect at least one rule in the netns, with the first entry at pref 100
		// pointing at table 255.
		rules, err := route.ListRules(netlink.FAMILY_V4, nil)
		assert.NoError(t, err)
		assert.GreaterOrEqual(t, len(rules), 1)
		assert.Equal(t, linux_defaults.RulePriorityLocalLookup, rules[0].Priority)
		assert.Equal(t, unix.RT_TABLE_LOCAL, rules[0].Table)

		rules, err = route.ListRules(netlink.FAMILY_V6, nil)
		assert.NoError(t, err)
		assert.GreaterOrEqual(t, len(rules), 1)
		assert.Equal(t, linux_defaults.RulePriorityLocalLookup, rules[0].Priority)
		assert.Equal(t, unix.RT_TABLE_LOCAL, rules[0].Table)
	}

	ns.Do(func() error {
		// Install rules the first time.
		test(t)

		// Ensure idempotency.
		test(t)

		return nil
	})
}

// recordingIPsecAgent wraps fakeipsec.Agent to record DeleteIPsecEndpoint
// calls, so tests can assert whether IPsec endpoint teardown happened
// without needing a real IPsec/XFRM setup.
type recordingIPsecAgent struct {
	fakeipsec.Agent
	deletedNodeIDs []uint16
}

func (a *recordingIPsecAgent) DeleteIPsecEndpoint(nodeID uint16) error {
	a.deletedNodeIDs = append(a.deletedNodeIDs, nodeID)
	return nil
}

// newTestNodeHandler builds a linuxNodeHandler suitable for exercising
// nodeDelete/deleteIPsec/node-ID bookkeeping directly, without any real
// netlink or BPF access (a fake node ID map is used instead).
func newTestNodeHandler(t testing.TB, ipsecAgent ipsecTypes.Agent) *linuxNodeHandler {
	t.Helper()
	log := hivetest.Logger(t)
	lns := node.NewTestLocalNodeStore(node.LocalNode{})
	handler := newNodeHandler(log, DatapathConfiguration{HostDevice: "host_device"}, nodemapfake.NewFakeNodeMapV2(), kpr.KPRConfig{}, ipsecAgent, fakeipsec.Config{}, lns)
	// Avoid the nil-func panic in nodeDelete/nodeUpdate; NodeConfigurationChanged
	// would set this too, but also drives netlink/xfrm side effects we don't
	// want in this unit test.
	handler.enableEncapsulation = func(*nodeTypes.Node) bool { return false }
	return handler
}

// Constellation: pawn CiliumNodes (perigeos host sharding) intentionally
// share the local node's CiliumInternalIP, and therefore the same allocated
// BPF node ID (see getNodeIDForNode's IP-based dedup). Deleting a pawn's
// CiliumNode must not release that shared ID - the physical host and its
// other pawns still need it. Regression test for the bug where nodeDelete
// only special-cased IsLocal() and unconditionally deallocated the ID for
// every other node, including managed pawns.
func TestNodeDeleteManagedPawnPreservesSharedNodeID(t *testing.T) {
	t.Cleanup(func() { nodeTypes.SetManagedNames(nil) })
	nodeTypes.SetManagedNames([]string{nodeTypes.GetName(), "pawn-2", "pawn-3"})

	handler := newTestNodeHandler(t, &fakeipsec.Agent{})

	sharedIP := net.ParseIP("192.168.50.10")
	primary := &nodeTypes.Node{
		Name:        nodeTypes.GetName(),
		IPAddresses: []nodeTypes.Address{{Type: nodeaddressing.NodeInternalIP, IP: sharedIP}},
	}
	pawn := &nodeTypes.Node{
		Name:        "pawn-2",
		IPAddresses: []nodeTypes.Address{{Type: nodeaddressing.NodeInternalIP, IP: sharedIP}},
	}

	id, err := handler.allocateIDForNode(nil, primary)
	require.NoError(t, err)
	require.NotZero(t, id)

	require.NoError(t, handler.nodeDelete(pawn))

	require.Equal(t, id, handler.getNodeIDForNode(primary),
		"deleting a managed pawn's CiliumNode must not deallocate the shared node ID")
	require.Contains(t, handler.nodeIDsByIPs, sharedIP.String())
}

// Baseline: deleting a genuinely unmanaged (real remote) node must still
// deallocate its node ID as before - the fix must not weaken cleanup for
// nodes that actually leave the cluster.
func TestNodeDeleteUnmanagedNodeDeallocatesNodeID(t *testing.T) {
	t.Cleanup(func() { nodeTypes.SetManagedNames(nil) })
	nodeTypes.SetManagedNames([]string{nodeTypes.GetName()})

	handler := newTestNodeHandler(t, &fakeipsec.Agent{})

	remote := &nodeTypes.Node{
		Name:        "some-other-cluster-node",
		IPAddresses: []nodeTypes.Address{{Type: nodeaddressing.NodeInternalIP, IP: net.ParseIP("10.0.0.5")}},
	}

	id, err := handler.allocateIDForNode(nil, remote)
	require.NoError(t, err)
	require.NotZero(t, id)

	require.NoError(t, handler.nodeDelete(remote))

	require.Zero(t, handler.getNodeIDForNode(remote))
	require.NotContains(t, handler.nodeIDsByIPs, "10.0.0.5")
}

// Same shared-ID concern for IPsec endpoint teardown: deleteIPsec calls
// DeleteIPsecEndpoint(nodeID) keyed by the same shared node ID, so it must
// also skip teardown for managed pawns while still tearing down IPsec state
// for genuinely departing remote nodes.
func TestDeleteIPsecSkipsEndpointTeardownForManagedPawn(t *testing.T) {
	t.Cleanup(func() { nodeTypes.SetManagedNames(nil) })
	nodeTypes.SetManagedNames([]string{nodeTypes.GetName(), "pawn-2"})

	agent := &recordingIPsecAgent{}
	handler := newTestNodeHandler(t, agent)

	sharedIP := net.ParseIP("192.168.50.10")
	primary := &nodeTypes.Node{
		Name:        nodeTypes.GetName(),
		IPAddresses: []nodeTypes.Address{{Type: nodeaddressing.NodeInternalIP, IP: sharedIP}},
	}
	pawn := &nodeTypes.Node{
		Name:        "pawn-2",
		IPAddresses: []nodeTypes.Address{{Type: nodeaddressing.NodeInternalIP, IP: sharedIP}},
	}

	_, err := handler.allocateIDForNode(nil, primary)
	require.NoError(t, err)

	require.NoError(t, handler.deleteIPsec(pawn))
	require.Empty(t, agent.deletedNodeIDs, "deleting a managed pawn must not tear down the shared IPsec endpoint")

	remote := &nodeTypes.Node{
		Name:        "some-other-cluster-node",
		IPAddresses: []nodeTypes.Address{{Type: nodeaddressing.NodeInternalIP, IP: net.ParseIP("10.0.0.5")}},
	}
	remoteID, err := handler.allocateIDForNode(nil, remote)
	require.NoError(t, err)

	require.NoError(t, handler.deleteIPsec(remote))
	require.Equal(t, []uint16{remoteID}, agent.deletedNodeIDs,
		"deleting a genuinely remote node must still tear down its IPsec endpoint")
}

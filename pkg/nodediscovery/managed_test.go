// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Constellation

package nodediscovery

import (
	"context"
	"net"
	"testing"

	"github.com/cilium/hive/hivetest"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	"github.com/cilium/cilium/pkg/k8s/client"
	clienttestutils "github.com/cilium/cilium/pkg/k8s/client/testutils"
	"github.com/cilium/cilium/pkg/node"
	nodeAddressing "github.com/cilium/cilium/pkg/node/addressing"
	nodeTypes "github.com/cilium/cilium/pkg/node/types"
)

// trackerGetters implements the k8sGetters interface by reading CiliumNodes
// back out of the fake clientset's object tracker. Routing reads through the
// same fake clientset that syncManagedCiliumInternalIP writes to keeps Get and
// Update consistent, so assertions observe the result of the Update.
type trackerGetters struct {
	cs client.Clientset
}

func (g trackerGetters) GetCiliumNode(ctx context.Context, name string) (*ciliumv2.CiliumNode, error) {
	return g.cs.CiliumV2().CiliumNodes().Get(ctx, name, metav1.GetOptions{})
}

// newManagedTestND builds a minimal NodeDiscovery wired to a fresh fake
// clientset pre-loaded with the given CiliumNodes, plus a counter of how many
// Update calls reach the API server (to assert the no-op fast path).
func newManagedTestND(t *testing.T, nodes ...*ciliumv2.CiliumNode) (*NodeDiscovery, *int) {
	t.Helper()
	fakeClient, _ := clienttestutils.NewFakeClientset(hivetest.Logger(t))
	for _, n := range nodes {
		require.NoError(t, fakeClient.CiliumFakeClientset.Tracker().Add(n))
	}

	updateCalls := 0
	fakeClient.CiliumFakeClientset.PrependReactor("update", "ciliumnodes",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			updateCalls++
			return false, nil, nil // fall through to the tracker's real Update
		},
	)

	nd := &NodeDiscovery{
		logger:     hivetest.Logger(t),
		clientset:  fakeClient,
		k8sGetters: trackerGetters{cs: fakeClient},
	}
	return nd, &updateCalls
}

// ciliumNode is a small builder for a CiliumNode carrying the given addresses.
func ciliumNode(name string, addrs ...ciliumv2.NodeAddress) *ciliumv2.CiliumNode {
	return &ciliumv2.CiliumNode{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       ciliumv2.NodeSpec{Addresses: addrs},
	}
}

// addrOfType returns the IP of the first address of the given type/family, or
// "" if none. family is "4" or "6".
func addrOfType(t *testing.T, cs client.Clientset, name string, typ nodeAddressing.AddressType, ipv6 bool) string {
	t.Helper()
	cn, err := cs.CiliumV2().CiliumNodes().Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	for _, a := range cn.Spec.Addresses {
		if a.Type != typ {
			continue
		}
		isV6 := net.ParseIP(a.IP).To4() == nil
		if isV6 == ipv6 {
			return a.IP
		}
	}
	return ""
}

// countAddrType counts addresses of a given type regardless of family.
func countAddrType(t *testing.T, cs client.Clientset, name string, typ nodeAddressing.AddressType) int {
	t.Helper()
	cn, err := cs.CiliumV2().CiliumNodes().Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	n := 0
	for _, a := range cn.Spec.Addresses {
		if a.Type == typ {
			n++
		}
	}
	return n
}

func internalIP(ip string) ciliumv2.NodeAddress {
	return ciliumv2.NodeAddress{Type: nodeAddressing.NodeInternalIP, IP: ip}
}

func ciliumInternalIP(ip string) ciliumv2.NodeAddress {
	return ciliumv2.NodeAddress{Type: nodeAddressing.NodeCiliumInternalIP, IP: ip}
}

// TestSyncManagedCiliumInternalIP_AppendsWhenAbsent verifies that the
// CiliumInternalIP (v4+v6) and HealthAddressing are added to a managed pawn
// CiliumNode that has none, while pre-existing fields (InternalIP, PodCIDRs)
// are preserved.
func TestSyncManagedCiliumInternalIP_AppendsWhenAbsent(t *testing.T) {
	pawn := ciliumNode("pawn-1", internalIP("192.168.0.10"))
	pawn.Spec.IPAM.PodCIDRs = []string{"10.10.0.0/24"}
	nd, _ := newManagedTestND(t, pawn)

	err := nd.syncManagedCiliumInternalIP(context.Background(), "pawn-1",
		"10.0.0.1", "fd00::1", "10.0.0.99", "fd00::99")
	require.NoError(t, err)

	require.Equal(t, "10.0.0.1", addrOfType(t, nd.clientset, "pawn-1", nodeAddressing.NodeCiliumInternalIP, false))
	require.Equal(t, "fd00::1", addrOfType(t, nd.clientset, "pawn-1", nodeAddressing.NodeCiliumInternalIP, true))

	cn, err := nd.clientset.CiliumV2().CiliumNodes().Get(context.Background(), "pawn-1", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "10.0.0.99", cn.Spec.HealthAddressing.IPv4)
	require.Equal(t, "fd00::99", cn.Spec.HealthAddressing.IPv6)
	// Preserved upstream/operator-owned fields.
	require.Equal(t, "192.168.0.10", addrOfType(t, nd.clientset, "pawn-1", nodeAddressing.NodeInternalIP, false))
	require.Equal(t, []string{"10.10.0.0/24"}, cn.Spec.IPAM.PodCIDRs)
}

// TestSyncManagedCiliumInternalIP_UpdatesInPlace verifies that an existing
// CiliumInternalIP of the same family is overwritten in place rather than a
// second address of the same family being appended.
func TestSyncManagedCiliumInternalIP_UpdatesInPlace(t *testing.T) {
	pawn := ciliumNode("pawn-1", ciliumInternalIP("10.0.0.1"))
	nd, _ := newManagedTestND(t, pawn)

	err := nd.syncManagedCiliumInternalIP(context.Background(), "pawn-1", "10.0.0.2", "", "", "")
	require.NoError(t, err)

	require.Equal(t, "10.0.0.2", addrOfType(t, nd.clientset, "pawn-1", nodeAddressing.NodeCiliumInternalIP, false))
	require.Equal(t, 1, countAddrType(t, nd.clientset, "pawn-1", nodeAddressing.NodeCiliumInternalIP),
		"same-family CiliumInternalIP must be replaced, not duplicated")
}

// TestSyncManagedCiliumInternalIP_NoopWhenUnchanged verifies the fast path: if
// every field already matches, no Update is issued to the API server.
func TestSyncManagedCiliumInternalIP_NoopWhenUnchanged(t *testing.T) {
	pawn := ciliumNode("pawn-1", ciliumInternalIP("10.0.0.1"))
	pawn.Spec.HealthAddressing.IPv4 = "10.0.0.99"
	nd, updateCalls := newManagedTestND(t, pawn)

	err := nd.syncManagedCiliumInternalIP(context.Background(), "pawn-1", "10.0.0.1", "", "10.0.0.99", "")
	require.NoError(t, err)
	require.Zero(t, *updateCalls, "no Update should be issued when nothing changed")
}

// TestSyncManagedCiliumInternalIP_EmptyIPsLeaveFamilyUntouched verifies that an
// empty IP string for a family is a no-op for that family (e.g. a v4-only host
// must not wipe a pawn's v6 CiliumInternalIP).
func TestSyncManagedCiliumInternalIP_EmptyIPsLeaveFamilyUntouched(t *testing.T) {
	pawn := ciliumNode("pawn-1", ciliumInternalIP("fd00::1"))
	nd, _ := newManagedTestND(t, pawn)

	err := nd.syncManagedCiliumInternalIP(context.Background(), "pawn-1", "10.0.0.1", "", "", "")
	require.NoError(t, err)

	require.Equal(t, "fd00::1", addrOfType(t, nd.clientset, "pawn-1", nodeAddressing.NodeCiliumInternalIP, true),
		"empty v6 must not remove the existing v6 CiliumInternalIP")
	require.Equal(t, "10.0.0.1", addrOfType(t, nd.clientset, "pawn-1", nodeAddressing.NodeCiliumInternalIP, false))
}

// localNodeWithCiliumIP builds a LocalNode advertising the given v4 cilium
// internal IP and v4 health IP.
func localNodeWithCiliumIP(name, ciliumV4, healthV4 string) *node.LocalNode {
	ln := &node.LocalNode{Node: nodeTypes.Node{Name: name}}
	if ciliumV4 != "" {
		ln.SetCiliumInternalIP(net.ParseIP(ciliumV4))
	}
	if healthV4 != "" {
		ln.IPv4HealthIP = net.ParseIP(healthV4)
	}
	return ln
}

// withManagedNames sets the managed node list (and local node name) for the
// duration of a test, restoring prior state on cleanup.
func withManagedNames(t *testing.T, local string, names []string) {
	t.Helper()
	savedName := nodeTypes.GetName()
	nodeTypes.SetName(local)
	nodeTypes.SetManagedNames(names)
	t.Cleanup(func() {
		nodeTypes.SetManagedNames(nil)
		nodeTypes.SetName(savedName)
	})
}

// TestUpdateManagedCiliumInternalIPs_PropagatesAndSkipsLocal verifies that the
// host's CiliumInternalIP is propagated onto every managed pawn except the
// local host node itself, which is left untouched.
func TestUpdateManagedCiliumInternalIPs_PropagatesAndSkipsLocal(t *testing.T) {
	withManagedNames(t, "host", []string{"host", "pawn-1", "pawn-2"})

	host := ciliumNode("host") // no CiliumInternalIP — must stay that way
	nd, _ := newManagedTestND(t, host, ciliumNode("pawn-1"), ciliumNode("pawn-2"))

	ln := localNodeWithCiliumIP("host", "10.0.0.1", "10.0.0.99")
	nd.updateManagedCiliumInternalIPs(context.Background(), ln)

	require.Equal(t, "10.0.0.1", addrOfType(t, nd.clientset, "pawn-1", nodeAddressing.NodeCiliumInternalIP, false))
	require.Equal(t, "10.0.0.1", addrOfType(t, nd.clientset, "pawn-2", nodeAddressing.NodeCiliumInternalIP, false))
	require.Empty(t, addrOfType(t, nd.clientset, "host", nodeAddressing.NodeCiliumInternalIP, false),
		"the local host node must be skipped, not updated")
}

// TestUpdateManagedCiliumInternalIPs_SingleNodeNoop verifies the standard
// (non-sharded) Cilium case: with a single managed name the whole routine is a
// no-op and never touches the API server.
func TestUpdateManagedCiliumInternalIPs_SingleNodeNoop(t *testing.T) {
	withManagedNames(t, "host", []string{"host"})

	nd, updateCalls := newManagedTestND(t, ciliumNode("host"))
	ln := localNodeWithCiliumIP("host", "10.0.0.1", "")
	nd.updateManagedCiliumInternalIPs(context.Background(), ln)

	require.Zero(t, *updateCalls, "single managed node must not trigger any update")
}

// TestUpdateManagedCiliumInternalIPs_NoCiliumIPNoop verifies that when the
// local node has no CiliumInternalIP yet (e.g. very early bootstrap) nothing is
// propagated — there is no source address to advertise.
func TestUpdateManagedCiliumInternalIPs_NoCiliumIPNoop(t *testing.T) {
	withManagedNames(t, "host", []string{"host", "pawn-1"})

	nd, updateCalls := newManagedTestND(t, ciliumNode("pawn-1"))
	ln := localNodeWithCiliumIP("host", "", "") // no cilium internal IP
	nd.updateManagedCiliumInternalIPs(context.Background(), ln)

	require.Zero(t, *updateCalls, "no source CiliumInternalIP means nothing to propagate")
}

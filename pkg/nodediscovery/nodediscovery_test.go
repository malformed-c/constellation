// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package nodediscovery

import (
	"context"
	"fmt"
	"testing"

	"github.com/cilium/hive/hivetest"
	"github.com/stretchr/testify/require"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	apimodels "github.com/cilium/cilium/api/v1/models"
	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	clienttestutils "github.com/cilium/cilium/pkg/k8s/client/testutils"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/node"
	nodeTypes "github.com/cilium/cilium/pkg/node/types"
	"github.com/cilium/cilium/pkg/option"
	cnitypes "github.com/cilium/cilium/plugins/cilium-cni/types"
)

// mockK8sGetters implements k8sGetters returning a pre-existing CiliumNode,
// so updateCiliumNodeResource takes the Update path (not Create).
type mockK8sGetters struct {
	ciliumNode *ciliumv2.CiliumNode
}

func (m *mockK8sGetters) GetCiliumNode(_ context.Context, _ string) (*ciliumv2.CiliumNode, error) {
	return m.ciliumNode, nil
}

// mockCNIConfigManager implements cni.CNIConfigManager with no-op methods.
type mockCNIConfigManager struct{}

func (m *mockCNIConfigManager) GetMTU() int                         { return 0 }
func (m *mockCNIConfigManager) GetChainingMode() string             { return "" }
func (m *mockCNIConfigManager) Status() *apimodels.Status           { return nil }
func (m *mockCNIConfigManager) GetCustomNetConf() *cnitypes.NetConf { return nil }
func (m *mockCNIConfigManager) ExternalRoutingEnabled() bool        { return false }

// newTestNodeDiscovery builds a minimal NodeDiscovery + LocalNode pair for
// the tests below, with AutoCreateCiliumNodeResource enabled and the given
// clientset/k8sGetters wired in.
func newTestNodeDiscovery(t *testing.T, nodeName string, clientset *clienttestutils.FakeClientset, getters k8sGetters) (*NodeDiscovery, *node.LocalNode) {
	t.Helper()
	option.Config.AutoCreateCiliumNodeResource = true
	option.Config.IPAM = ""
	t.Cleanup(func() {
		option.Config.AutoCreateCiliumNodeResource = false
	})
	nodeTypes.SetName(nodeName)

	nd := &NodeDiscovery{
		logger:           hivetest.Logger(t),
		clientset:        clientset,
		k8sGetters:       getters,
		cniConfigManager: &mockCNIConfigManager{},
	}
	ln := &node.LocalNode{
		Node:  nodeTypes.Node{Name: nodeName},
		Local: &node.LocalNodeInfo{},
	}
	return nd, ln
}

// TestUpdateCiliumNodeResourceTransientErrorRetriesThenReturnsError reproduces
// https://github.com/cilium/cilium/issues/44388: a transient error from the
// API server during a CiliumNode Update caused logging.Fatal instead of being
// retried. updateCiliumNodeResource now returns the error to its caller
// instead of calling logging.Fatal itself - see
// TestUpdateCiliumNodeResourcePublicWrapperStillFatals for that behavior,
// which is preserved for the one remaining caller that needs it.
func TestUpdateCiliumNodeResourceTransientErrorRetriesThenReturnsError(t *testing.T) {
	const nodeName = "test-node"
	fakeClient, _ := clienttestutils.NewFakeClientset(hivetest.Logger(t))

	existingNode := &ciliumv2.CiliumNode{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
	}
	require.NoError(t, fakeClient.CiliumFakeClientset.Tracker().Add(existingNode))

	updateCalls := 0
	// PrependReactor is required: the tracker's ObjectReaction (added at
	// clientset creation via AddReactor) would otherwise intercept Update
	// first and return a Conflict error due to the missing resource version.
	fakeClient.CiliumFakeClientset.PrependReactor("update", "ciliumnodes",
		func(_ k8stesting.Action) (bool, runtime.Object, error) {
			updateCalls++
			return true, nil, fmt.Errorf("connection reset by peer")
		},
	)

	nd, ln := newTestNodeDiscovery(t, nodeName, fakeClient, &mockK8sGetters{ciliumNode: existingNode})

	err := nd.updateCiliumNodeResource(context.Background(), ln)
	require.Error(t, err)
	require.Equal(t, maxRetryCount, updateCalls,
		"transient errors should be retried %d times, not fataled immediately", maxRetryCount)
}

// TestUpdateCiliumNodeResourcePublicWrapperStillFatals verifies the public
// UpdateCiliumNodeResource - used by the pkg/ipam/crd.go CRD-mode Owner
// callback, which has no error-propagation path of its own - still calls
// logging.Fatal on exhaustion, preserving its existing contract. Only the
// new TryUpdateCiliumNodeResource (used by the daemon start hook) returns
// the error instead.
func TestUpdateCiliumNodeResourcePublicWrapperStillFatals(t *testing.T) {
	logging.RegisterExitHandler(func() { panic("fatal called") })
	t.Cleanup(func() { logging.RegisterExitHandler(func() {}) })

	const nodeName = "test-node"
	fakeClient, _ := clienttestutils.NewFakeClientset(hivetest.Logger(t))

	existingNode := &ciliumv2.CiliumNode{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
	}
	require.NoError(t, fakeClient.CiliumFakeClientset.Tracker().Add(existingNode))

	fakeClient.CiliumFakeClientset.PrependReactor("update", "ciliumnodes",
		func(_ k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, fmt.Errorf("connection reset by peer")
		},
	)

	nd, ln := newTestNodeDiscovery(t, nodeName, fakeClient, &mockK8sGetters{ciliumNode: existingNode})
	nd.localNodeStore = node.NewTestLocalNodeStore(*ln)

	require.Panics(t, func() {
		nd.UpdateCiliumNodeResource()
	})
}

// TestUpdateCiliumNodeResourceConflictRereadsFromAPIServerNotCache is a
// regression test for the actual live bug: on a CiliumNode Update conflict,
// the retry loop used to re-GET via k8sGetters, which reads from the local
// informer store whenever it's initialized - including the agent's own
// just-made write, which the local watch has no guarantee of having
// observed yet. A stale cached read reproduces the identical conflict
// forever instead of resolving it. k8sGetters here always returns a
// deliberately stale object (an old resourceVersion) to prove the retry
// path doesn't depend on it recovering - only a direct apiserver read (via
// the clientset) can converge.
func TestUpdateCiliumNodeResourceConflictRereadsFromAPIServerNotCache(t *testing.T) {
	const nodeName = "test-node"
	fakeClient, _ := clienttestutils.NewFakeClientset(hivetest.Logger(t))

	existingNode := &ciliumv2.CiliumNode{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
	}
	require.NoError(t, fakeClient.CiliumFakeClientset.Tracker().Add(existingNode))

	// The cache's view: fixed and never changes across calls. If the retry
	// path ever consults it again after the first attempt, that alone means
	// the fix regressed - this test doesn't need it to ever "catch up" to
	// prove that, since real convergence only depends on the apiserver read.
	getters := &countingK8sGetters{ciliumNode: existingNode}

	// First Update() attempt conflicts (simulating another writer having
	// just landed a change, e.g. the agent's own earlier write per the
	// read-your-own-writes finding); every attempt after that succeeds.
	// This mirrors the existing transient-error test's technique
	// (PrependReactor controlling the outcome by call count) rather than
	// trying to also simulate the fake tracker's own resourceVersion
	// bookkeeping, which is orthogonal to what this test is actually
	// checking: which read path (cache vs apiserver) each retry uses.
	updateCalls := 0
	fakeClient.CiliumFakeClientset.PrependReactor("update", "ciliumnodes",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			updateCalls++
			if updateCalls == 1 {
				return true, nil, k8serrors.NewConflict(
					schema.GroupResource{Group: "cilium.io", Resource: "ciliumnodes"},
					nodeName, fmt.Errorf("the object has been modified; please apply your changes to the latest version"))
			}
			obj := action.(k8stesting.UpdateAction).GetObject()
			return true, obj, nil
		},
	)

	nd, ln := newTestNodeDiscovery(t, nodeName, fakeClient, getters)

	err := nd.updateCiliumNodeResource(context.Background(), ln)
	require.NoError(t, err, "must converge once it re-reads from the apiserver instead of repeating the cached read")
	require.Equal(t, 2, updateCalls, "must succeed on the second attempt, not exhaust retries")
	require.Equal(t, 1, getters.calls,
		"k8sGetters (the cache) must only be consulted on the FIRST attempt - every retry after a conflict must read the apiserver directly")
}

// countingK8sGetters implements k8sGetters returning a fixed (stale)
// CiliumNode, and counts how many times it was consulted.
type countingK8sGetters struct {
	ciliumNode *ciliumv2.CiliumNode
	calls      int
}

func (m *countingK8sGetters) GetCiliumNode(_ context.Context, _ string) (*ciliumv2.CiliumNode, error) {
	m.calls++
	return m.ciliumNode, nil
}

// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Constellation

package k8s

// Tests for the --managed-nodes pod reflector behaviour introduced for the
// perigeos host-sharding topology, where one constellation-agent manages pods
// across multiple virtual k8s nodes (pawns) on a single physical host.
//
// Each test starts a real hive with a fake k8s client and asserts on the
// statedb LocalPod table rather than mocking the reflector layer.

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cilium/hive/cell"
	"github.com/cilium/hive/hivetest"
	"github.com/cilium/statedb"
	"github.com/stretchr/testify/require"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/cilium/cilium/pkg/hive"
	k8sTestUtils "github.com/cilium/cilium/pkg/k8s/client/testutils"
	slim_corev1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/core/v1"
	slim_metav1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/apis/meta/v1"

	nodeTypes "github.com/cilium/cilium/pkg/node/types"
	"github.com/cilium/cilium/pkg/option"
)

const testTimeout = 10 * time.Second

// slimPod builds a minimal slim Pod for a given node.
func slimPod(name, namespace, nodeName, uid string) *slim_corev1.Pod {
	return &slim_corev1.Pod{
		ObjectMeta: slim_metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			UID:             k8stypes.UID(uid),
			ResourceVersion: "1",
		},
		Spec: slim_corev1.PodSpec{
			NodeName: nodeName,
		},
	}
}

// podExists checks whether a pod is present in the LocalPod table.
func podExists(t *testing.T, tbl statedb.Table[LocalPod], db *statedb.DB, namespace, name string) bool {
	t.Helper()
	_, _, ok := tbl.Get(db.ReadTxn(), PodByName(namespace, name))
	return ok
}

// managedNodesHiveWithTable is the primary helper — it captures the typed
// Table[LocalPod] so tests can call table.Get and table.NumObjects directly.
func managedNodesHiveWithTable(t *testing.T) (statedb.Table[LocalPod], *statedb.DB, *k8sTestUtils.FakeClientset) {
	t.Helper()

	var (
		db  *statedb.DB
		cs  *k8sTestUtils.FakeClientset
		tbl statedb.Table[LocalPod]
	)

	h := hive.New(
		k8sTestUtils.FakeClientCell(),
		TablesCell,
		cell.Invoke(func(t statedb.Table[LocalPod]) {
			tbl = t
		}),
		cell.Invoke(func(d *statedb.DB, c *k8sTestUtils.FakeClientset) {
			db = d
			cs = c
		}),
	)

	log := hivetest.Logger(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)

	require.NoError(t, h.Start(log, ctx))
	t.Cleanup(func() {
		require.NoError(t, h.Stop(log, context.Background()))
	})

	return tbl, db, cs
}

// ─── helper: reset managed names after each test ─────────────────────────────

func clearManagedNames(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { nodeTypes.SetManagedNames(nil) })
}

// ─── Tests ────────────────────────────────────────────────────────────────────

// TestManagedNodes_SinglePawn_DefaultBehaviour verifies that the zero-value
// case (no SetManagedNames call) behaves identically to stock Cilium: only
// pods on the local node appear in the table.
func TestManagedNodes_SinglePawn_DefaultBehaviour(t *testing.T) {
	clearManagedNames(t)
	nodeTypes.SetName("host-01")
	// GetManagedNames() returns ["host-01"] — one reflector, same as upstream.

	tbl, db, cs := managedNodesHiveWithTable(t)
	ctx := t.Context()

	_, err := cs.Slim().CoreV1().Pods("default").Create(ctx,
		slimPod("nginx", "default", "host-01", "uid-01"), meta_v1.CreateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return podExists(t, tbl, db, "default", "nginx")
	}, 5*time.Second, 20*time.Millisecond, "pod on local node must appear in table")

	// A pod on a different host must be filtered by the field selector.
	_, err = cs.Slim().CoreV1().Pods("default").Create(ctx,
		slimPod("stranger", "default", "other-host", "uid-02"), meta_v1.CreateOptions{})
	require.NoError(t, err)

	time.Sleep(150 * time.Millisecond)
	require.False(t, podExists(t, tbl, db, "default", "stranger"),
		"pod on unmanaged host must not appear")
}

// TestManagedNodes_TwoPawns_BothPodsVisible is the core perigeos scenario:
// one constellation-agent instance managing two pawn nodes on the same host.
// Pods scheduled on either pawn must both end up in the LocalPod table.
func TestManagedNodes_TwoPawns_BothPodsVisible(t *testing.T) {
	clearManagedNames(t)
	nodeTypes.SetName("rack-01")
	nodeTypes.SetManagedNames([]string{"pawn-0", "pawn-1"})

	tbl, db, cs := managedNodesHiveWithTable(t)
	ctx := t.Context()

	_, err := cs.Slim().CoreV1().Pods("prod").Create(ctx,
		slimPod("frontend", "prod", "pawn-0", "uid-fe"), meta_v1.CreateOptions{})
	require.NoError(t, err)
	_, err = cs.Slim().CoreV1().Pods("prod").Create(ctx,
		slimPod("backend", "prod", "pawn-1", "uid-be"), meta_v1.CreateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return podExists(t, tbl, db, "prod", "frontend") &&
			podExists(t, tbl, db, "prod", "backend")
	}, 5*time.Second, 20*time.Millisecond,
		"pods on both managed pawns must appear in LocalPod table")
}

// TestManagedNodes_ForeignPawnExcluded verifies that with two managed pawns, a
// pod on a third pawn — whose name shares a common prefix — is not reflected.
// This guards against accidental prefix-match bugs in the field selector.
func TestManagedNodes_ForeignPawnExcluded(t *testing.T) {
	clearManagedNames(t)
	nodeTypes.SetName("rack-02")
	nodeTypes.SetManagedNames([]string{"pawn-0", "pawn-1"})

	tbl, db, cs := managedNodesHiveWithTable(t)
	ctx := t.Context()

	_, err := cs.Slim().CoreV1().Pods("default").Create(ctx,
		slimPod("on-pawn-0", "default", "pawn-0", "uid-p0"), meta_v1.CreateOptions{})
	require.NoError(t, err)
	_, err = cs.Slim().CoreV1().Pods("default").Create(ctx,
		slimPod("on-pawn-2", "default", "pawn-2", "uid-p2"), meta_v1.CreateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return podExists(t, tbl, db, "default", "on-pawn-0")
	}, 5*time.Second, 20*time.Millisecond, "managed pod must appear")

	time.Sleep(150 * time.Millisecond)
	require.False(t, podExists(t, tbl, db, "default", "on-pawn-2"),
		"pod on unmanaged pawn-2 must not appear even though name shares prefix")
}

// TestManagedNodes_PodDeletion verifies that deleting a pod from a managed
// pawn removes it from the LocalPod table while leaving the other pawn's pod
// untouched.
func TestManagedNodes_PodDeletion(t *testing.T) {
	clearManagedNames(t)
	nodeTypes.SetName("rack-03")
	nodeTypes.SetManagedNames([]string{"pawn-a", "pawn-b"})

	tbl, db, cs := managedNodesHiveWithTable(t)
	ctx := t.Context()

	for _, p := range []*slim_corev1.Pod{
		slimPod("web", "default", "pawn-a", "uid-web"),
		slimPod("cache", "default", "pawn-b", "uid-cache"),
	} {
		_, err := cs.Slim().CoreV1().Pods("default").Create(ctx, p, meta_v1.CreateOptions{})
		require.NoError(t, err)
	}

	require.Eventually(t, func() bool {
		return podExists(t, tbl, db, "default", "web") &&
			podExists(t, tbl, db, "default", "cache")
	}, 5*time.Second, 20*time.Millisecond, "both pods must appear before deletion test")

	require.NoError(t,
		cs.Slim().CoreV1().Pods("default").Delete(ctx, "web", meta_v1.DeleteOptions{}))

	require.Eventually(t, func() bool {
		return !podExists(t, tbl, db, "default", "web")
	}, 5*time.Second, 20*time.Millisecond, "deleted pod must leave the table")

	require.True(t, podExists(t, tbl, db, "default", "cache"),
		"pod on the surviving pawn must remain in table")
}

// TestManagedNodes_ThreePawns_AllPodsPresent verifies that three independent
// reflectors (one per pawn) all write into the same LocalPod table without
// losing or duplicating entries.
func TestManagedNodes_ThreePawns_AllPodsPresent(t *testing.T) {
	clearManagedNames(t)
	nodeTypes.SetName("rack-04")
	nodeTypes.SetManagedNames([]string{"pawn-0", "pawn-1", "pawn-2"})

	tbl, db, cs := managedNodesHiveWithTable(t)
	ctx := t.Context()

	pods := []*slim_corev1.Pod{
		slimPod("alpha", "ns", "pawn-0", "uid-alpha"),
		slimPod("beta", "ns", "pawn-1", "uid-beta"),
		slimPod("gamma", "ns", "pawn-2", "uid-gamma"),
	}
	for _, p := range pods {
		_, err := cs.Slim().CoreV1().Pods("ns").Create(ctx, p, meta_v1.CreateOptions{})
		require.NoError(t, err)
	}

	require.Eventually(t, func() bool {
		return podExists(t, tbl, db, "ns", "alpha") &&
			podExists(t, tbl, db, "ns", "beta") &&
			podExists(t, tbl, db, "ns", "gamma")
	}, 5*time.Second, 20*time.Millisecond,
		"pods from all three pawns must appear in table")

	require.Equal(t, 3, tbl.NumObjects(db.ReadTxn()),
		"table must contain exactly the three managed pods, no extras")
}

// ─── Selector-based discovery helpers ────────────────────────────────────────

// slimNode builds a minimal slim Node with the given name and labels.
func slimNode(name string, labels map[string]string) *slim_corev1.Node {
	return &slim_corev1.Node{
		ObjectMeta: slim_metav1.ObjectMeta{
			Name:            name,
			Labels:          labels,
			ResourceVersion: "1",
		},
	}
}

// clearSelector resets option.Config.ManagedNodesSelector after the test.
func clearSelector(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { option.Config.ManagedNodesSelector = "" })
}

// selectorHiveWithTable builds a hive with option.Config.ManagedNodesSelector
// set. Pre-creates Node objects in the fake clientset before hive starts so
// that discoverManagedNodes can find them during Provide.
func selectorHiveWithTable(
	t *testing.T,
	selector string,
	nodes []*slim_corev1.Node,
) (statedb.Table[LocalPod], *statedb.DB, *k8sTestUtils.FakeClientset) {
	t.Helper()

	// Set the selector BEFORE hive construction — NewPodTableAndReflector
	// reads it during Provide.
	option.Config.ManagedNodesSelector = selector

	var (
		db  *statedb.DB
		cs  *k8sTestUtils.FakeClientset
		tbl statedb.Table[LocalPod]
	)

	h := hive.New(
		k8sTestUtils.FakeClientCell(),
		// Pre-populate nodes before TablesCell registers reflectors.
		cell.Invoke(func(c *k8sTestUtils.FakeClientset) {
			cs = c
			ctx := context.Background()
			for _, n := range nodes {
				_, err := c.Slim().CoreV1().Nodes().Create(ctx, n, meta_v1.CreateOptions{})
				require.NoError(t, err)
			}
		}),
		TablesCell,
		cell.Invoke(func(t statedb.Table[LocalPod]) {
			tbl = t
		}),
		cell.Invoke(func(d *statedb.DB) {
			db = d
		}),
	)

	log := hivetest.Logger(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)

	require.NoError(t, h.Start(log, ctx))
	t.Cleanup(func() {
		require.NoError(t, h.Stop(log, context.Background()))
	})

	return tbl, db, cs
}

// ─── Selector-based discovery tests ──────────────────────────────────────────

// TestManagedNodesSelector_DiscoversLabeledNodes verifies the core selector
// path: nodes matching the label selector are discovered, their pods appear
// in the table, and unlabeled nodes are excluded.
func TestManagedNodesSelector_DiscoversLabeledNodes(t *testing.T) {
	clearManagedNames(t)
	clearSelector(t)
	nodeTypes.SetName("host-sel-1")

	nodes := []*slim_corev1.Node{
		slimNode("pawn-a", map[string]string{"perigeos.io/host": "rack-10"}),
		slimNode("pawn-b", map[string]string{"perigeos.io/host": "rack-10"}),
		slimNode("other-host", map[string]string{"unrelated": "label"}),
	}

	tbl, db, cs := selectorHiveWithTable(t, "perigeos.io/host=rack-10", nodes)
	ctx := t.Context()

	// Create pods on all three nodes.
	for _, p := range []*slim_corev1.Pod{
		slimPod("app-a", "default", "pawn-a", "uid-sel-a"),
		slimPod("app-b", "default", "pawn-b", "uid-sel-b"),
		slimPod("app-other", "default", "other-host", "uid-sel-other"),
	} {
		_, err := cs.Slim().CoreV1().Pods("default").Create(ctx, p, meta_v1.CreateOptions{})
		require.NoError(t, err)
	}

	// Pods on labeled nodes must appear.
	require.Eventually(t, func() bool {
		return podExists(t, tbl, db, "default", "app-a") &&
			podExists(t, tbl, db, "default", "app-b")
	}, 5*time.Second, 20*time.Millisecond,
		"pods on labeled nodes must appear in LocalPod table")

	// Pod on unlabeled node must NOT appear.
	time.Sleep(150 * time.Millisecond)
	require.False(t, podExists(t, tbl, db, "default", "app-other"),
		"pod on unlabeled node must not appear")

	// IsManaged must reflect discovered nodes.
	require.True(t, nodeTypes.IsManaged("pawn-a"))
	require.True(t, nodeTypes.IsManaged("pawn-b"))
	require.False(t, nodeTypes.IsManaged("other-host"))
}

// TestManagedNodesSelector_EmptySelector_DefaultBehaviour verifies that an
// empty selector behaves identically to stock Cilium: only the local node's
// pods appear.
func TestManagedNodesSelector_EmptySelector_DefaultBehaviour(t *testing.T) {
	clearManagedNames(t)
	clearSelector(t)
	nodeTypes.SetName("local-host")

	// Empty selector — use the standard managedNodesHiveWithTable helper
	// (no selector logic, no node pre-population needed).
	tbl, db, cs := managedNodesHiveWithTable(t)
	ctx := t.Context()

	_, err := cs.Slim().CoreV1().Pods("default").Create(ctx,
		slimPod("local-pod", "default", "local-host", "uid-local"), meta_v1.CreateOptions{})
	require.NoError(t, err)
	_, err = cs.Slim().CoreV1().Pods("default").Create(ctx,
		slimPod("remote-pod", "default", "remote-host", "uid-remote"), meta_v1.CreateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return podExists(t, tbl, db, "default", "local-pod")
	}, 5*time.Second, 20*time.Millisecond, "local pod must appear")

	time.Sleep(150 * time.Millisecond)
	require.False(t, podExists(t, tbl, db, "default", "remote-pod"),
		"remote pod must not appear in standard single-node mode")
}

// TestManagedNodesSelector_NoMatchingNodes_FallsBackToLocal verifies that
// when the selector matches no nodes, the agent falls back to managing
// the local node only.
func TestManagedNodesSelector_NoMatchingNodes_FallsBackToLocal(t *testing.T) {
	clearManagedNames(t)
	clearSelector(t)
	nodeTypes.SetName("local-host")

	// No nodes match the selector.
	tbl, db, cs := selectorHiveWithTable(t, "perigeos.io/host=nonexistent", nil)
	ctx := t.Context()

	_, err := cs.Slim().CoreV1().Pods("default").Create(ctx,
		slimPod("fallback-pod", "default", "local-host", "uid-fallback"), meta_v1.CreateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return podExists(t, tbl, db, "default", "fallback-pod")
	}, 5*time.Second, 20*time.Millisecond,
		"pod on local node must appear via fallback")

	require.True(t, nodeTypes.IsManaged("local-host"),
		"local node must be managed after fallback")
}

// TestManagedNodesSelector_DynamicNodeAdd verifies that when a new Node
// object matching the selector appears at runtime, the node watcher
// detects it and registers a pod reflector for it.
func TestManagedNodesSelector_DynamicNodeAdd(t *testing.T) {
	clearManagedNames(t)
	clearSelector(t)
	nodeTypes.SetName("host-dyn")

	// Start with one node.
	nodes := []*slim_corev1.Node{
		slimNode("pawn-x", map[string]string{"perigeos.io/host": "rack-20"}),
	}
	tbl, db, cs := selectorHiveWithTable(t, "perigeos.io/host=rack-20", nodes)
	ctx := t.Context()

	// Pod on initial node.
	_, err := cs.Slim().CoreV1().Pods("default").Create(ctx,
		slimPod("pod-x", "default", "pawn-x", "uid-dx"), meta_v1.CreateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return podExists(t, tbl, db, "default", "pod-x")
	}, 5*time.Second, 20*time.Millisecond, "pod on initial node must appear")

	// Dynamically add a new node matching the selector.
	_, err = cs.Slim().CoreV1().Nodes().Create(ctx,
		slimNode("pawn-y", map[string]string{"perigeos.io/host": "rack-20"}),
		meta_v1.CreateOptions{})
	require.NoError(t, err)

	// Wait for the node watcher to detect the new node.
	require.Eventually(t, func() bool {
		return nodeTypes.IsManaged("pawn-y")
	}, 5*time.Second, 50*time.Millisecond,
		"dynamically added node must become managed")

	// Pod on the new node.
	_, err = cs.Slim().CoreV1().Pods("default").Create(ctx,
		slimPod("pod-y", "default", "pawn-y", "uid-dy"), meta_v1.CreateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return podExists(t, tbl, db, "default", "pod-y")
	}, 5*time.Second, 50*time.Millisecond,
		"pod on dynamically added node must appear in table")

	// Both pods coexist.
	require.True(t, podExists(t, tbl, db, "default", "pod-x"),
		"pod on initial node must still be present")
}

// TestManagedNodesSelector_NodeRemoval verifies that when a Node object
// matching the selector is deleted, the node is removed from the managed
// set and IsManaged returns false.
func TestManagedNodesSelector_NodeRemoval(t *testing.T) {
	clearManagedNames(t)
	clearSelector(t)
	nodeTypes.SetName("host-rm")

	nodes := []*slim_corev1.Node{
		slimNode("pawn-1", map[string]string{"perigeos.io/host": "rack-30"}),
		slimNode("pawn-2", map[string]string{"perigeos.io/host": "rack-30"}),
	}
	tbl, db, cs := selectorHiveWithTable(t, "perigeos.io/host=rack-30", nodes)
	ctx := t.Context()

	// Create pods on both nodes.
	for _, p := range []*slim_corev1.Pod{
		slimPod("pod-1", "default", "pawn-1", "uid-rm1"),
		slimPod("pod-2", "default", "pawn-2", "uid-rm2"),
	} {
		_, err := cs.Slim().CoreV1().Pods("default").Create(ctx, p, meta_v1.CreateOptions{})
		require.NoError(t, err)
	}

	require.Eventually(t, func() bool {
		return podExists(t, tbl, db, "default", "pod-1") &&
			podExists(t, tbl, db, "default", "pod-2")
	}, 5*time.Second, 20*time.Millisecond, "pods on both nodes must appear")

	// Delete pawn-2 node.
	require.NoError(t,
		cs.Slim().CoreV1().Nodes().Delete(ctx, "pawn-2", meta_v1.DeleteOptions{}))

	// Node watcher should remove pawn-2 from managed set.
	require.Eventually(t, func() bool {
		return !nodeTypes.IsManaged("pawn-2")
	}, 5*time.Second, 50*time.Millisecond,
		"deleted node must no longer be managed")

	// pawn-1 must remain managed.
	require.True(t, nodeTypes.IsManaged("pawn-1"),
		"surviving node must remain managed")

	// Pod on pawn-1 must still be in the table.
	require.True(t, podExists(t, tbl, db, "default", "pod-1"),
		"pod on surviving node must remain in table")
}

// ─── NodeAddedCallback tests ─────────────────────────────────────────────────

// clearNodeCallbacks resets the global node-added callback list after test.
func clearNodeCallbacks(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { ResetNodeAddedCallbacks() })
}

// TestNodeAddedCallback_FiredOnDynamicAdd verifies that registered
// NodeAddedCallbacks are invoked when the node watcher detects a new node.
// This is the mechanism IPAM uses to dynamically add per-pawn sub-allocators.
func TestNodeAddedCallback_FiredOnDynamicAdd(t *testing.T) {
	clearManagedNames(t)
	clearSelector(t)
	clearNodeCallbacks(t)
	nodeTypes.SetName("host-cb")

	// Register a callback that records added node names.
	var (
		mu    sync.Mutex
		added []string
	)
	RegisterNodeAddedCallback(func(name string) {
		mu.Lock()
		defer mu.Unlock()
		added = append(added, name)
	})

	// Start with one node.
	nodes := []*slim_corev1.Node{
		slimNode("pawn-init", map[string]string{"perigeos.io/host": "rack-cb"}),
	}
	_, _, cs := selectorHiveWithTable(t, "perigeos.io/host=rack-cb", nodes)
	ctx := t.Context()

	// Dynamically add a new node.
	_, err := cs.Slim().CoreV1().Nodes().Create(ctx,
		slimNode("pawn-late", map[string]string{"perigeos.io/host": "rack-cb"}),
		meta_v1.CreateOptions{})
	require.NoError(t, err)

	// Wait for the callback to fire.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(added, "pawn-late")
	}, 5*time.Second, 50*time.Millisecond,
		"NodeAddedCallback must be invoked for dynamically added node")

	// The initial node should NOT have triggered the callback (it was
	// already known at startup).
	mu.Lock()
	for _, n := range added {
		if n == "pawn-init" {
			t.Error("callback should not fire for nodes already known at startup")
		}
	}
	mu.Unlock()
}

// TestNodeAddedCallback_MultipleCallbacks verifies that multiple registered
// callbacks are all invoked when a new node is added.
func TestNodeAddedCallback_MultipleCallbacks(t *testing.T) {
	clearManagedNames(t)
	clearSelector(t)
	clearNodeCallbacks(t)
	nodeTypes.SetName("host-mcb")

	var (
		mu     sync.Mutex
		count1 int
		count2 int
	)
	RegisterNodeAddedCallback(func(name string) {
		mu.Lock()
		defer mu.Unlock()
		count1++
	})
	RegisterNodeAddedCallback(func(name string) {
		mu.Lock()
		defer mu.Unlock()
		count2++
	})

	nodes := []*slim_corev1.Node{
		slimNode("pawn-a", map[string]string{"perigeos.io/host": "rack-mcb"}),
	}
	_, _, cs := selectorHiveWithTable(t, "perigeos.io/host=rack-mcb", nodes)
	ctx := t.Context()

	_, err := cs.Slim().CoreV1().Nodes().Create(ctx,
		slimNode("pawn-b", map[string]string{"perigeos.io/host": "rack-mcb"}),
		meta_v1.CreateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return count1 > 0 && count2 > 0
	}, 5*time.Second, 50*time.Millisecond,
		"both registered callbacks must be invoked")
}

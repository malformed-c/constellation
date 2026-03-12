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
	"testing"
	"time"

	"github.com/cilium/hive/cell"
	"github.com/cilium/hive/hivetest"
	"github.com/cilium/statedb"
	"github.com/stretchr/testify/require"

	"github.com/cilium/cilium/pkg/hive"
	k8sTestUtils "github.com/cilium/cilium/pkg/k8s/client/testutils"
	slim_corev1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/core/v1"
	slim_metav1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/apis/meta/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	nodeTypes "github.com/cilium/cilium/pkg/node/types"
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

// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package podendpointwatchdog

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/cilium/hive/hivetest"
	"github.com/cilium/statedb"
	"github.com/stretchr/testify/require"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/cilium/cilium/pkg/endpoint"
	slim_corev1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/core/v1"
	slim_metav1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/apis/meta/v1"
	"github.com/cilium/cilium/pkg/k8s/tables"
	"github.com/cilium/cilium/pkg/time"
)

type fakeEndpointLookup struct {
	byIP map[string]*endpoint.Endpoint
}

func (f *fakeEndpointLookup) LookupIP(ip netip.Addr) *endpoint.Endpoint {
	return f.byIP[ip.String()]
}

type fakePodDeleter struct {
	deleted []string
	err     error
}

func (f *fakePodDeleter) DeletePod(_ context.Context, namespace, name string, _ k8stypes.UID) error {
	f.deleted = append(f.deleted, namespace+"/"+name)
	return f.err
}

// fakeClock lets tests control the passage of time deterministically,
// since the grace period is measured against real elapsed time between
// scans, not any pod-provided timestamp (see watchdog.pending's doc
// comment for why).
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }
func newFakeClock() *fakeClock               { return &fakeClock{now: time.Now()} }

func newTestWatchdog(t *testing.T, gracePeriod time.Duration) (*watchdog, statedb.RWTable[tables.LocalPod], *statedb.DB, *fakeEndpointLookup, *fakePodDeleter, *fakeClock) {
	t.Helper()

	db := statedb.New()
	podTable, err := tables.NewPodTable(db)
	require.NoError(t, err)

	eps := &fakeEndpointLookup{byIP: map[string]*endpoint.Endpoint{}}
	deleter := &fakePodDeleter{}
	clock := newFakeClock()

	w := &watchdog{
		logger:      hivetest.Logger(t),
		db:          db,
		podTable:    podTable,
		endpoints:   eps,
		podDeleter:  deleter,
		gracePeriod: gracePeriod,
		now:         clock.Now,
		pending:     make(map[k8stypes.UID]time.Time),
		// Existing tests exercise the healing logic, so they run as a
		// cilium-managed node. The gate itself is covered separately by
		// TestCheck_StandsDownWhenNodeUsesAnotherCNI.
		cniOwned: func(context.Context) (bool, string) { return true, "test: cilium-managed" },
	}
	return w, podTable, db, eps, deleter, clock
}

func insertPod(t *testing.T, db *statedb.DB, podTable statedb.RWTable[tables.LocalPod], pod tables.LocalPod) {
	t.Helper()
	txn := db.WriteTxn(podTable)
	_, _, err := podTable.Insert(txn, pod)
	require.NoError(t, err)
	txn.Commit()
}

// runningPod builds a Running pod with no status.startTime set, matching
// control planes (like the one that motivated this package) that never
// populate it. The watchdog must not depend on that field.
func runningPod(uid k8stypes.UID, namespace, name, ip string, hostNetwork bool) tables.LocalPod {
	return tables.LocalPod{Pod: &slim_corev1.Pod{
		ObjectMeta: slim_metav1.ObjectMeta{
			UID:       uid,
			Namespace: namespace,
			Name:      name,
		},
		Spec: slim_corev1.PodSpec{
			HostNetwork: hostNetwork,
		},
		Status: slim_corev1.PodStatus{
			Phase: slim_corev1.PodRunning,
			PodIP: ip,
		},
	}}
}

func TestWatchdog_HealthyPodNeverDeleted(t *testing.T) {
	w, podTable, db, eps, deleter, clock := newTestWatchdog(t, time.Minute)
	pod := runningPod("uid-1", "default", "healthy", "10.0.0.1", false)
	insertPod(t, db, podTable, pod)

	addr := netip.MustParseAddr("10.0.0.1")
	eps.byIP[addr.String()] = &endpoint.Endpoint{}

	require.NoError(t, w.check(context.Background()))
	clock.Advance(time.Hour)
	require.NoError(t, w.check(context.Background()))

	require.Empty(t, deleter.deleted)
}

func TestWatchdog_HostNetworkPodNeverDeleted(t *testing.T) {
	w, podTable, db, _, deleter, clock := newTestWatchdog(t, time.Minute)
	pod := runningPod("uid-2", "kube-system", "host-net", "10.0.0.2", true)
	insertPod(t, db, podTable, pod)

	require.NoError(t, w.check(context.Background()))
	clock.Advance(time.Hour)
	require.NoError(t, w.check(context.Background()))

	require.Empty(t, deleter.deleted, "hostNetwork pods never get their own Cilium endpoint and must never be healed")
}

func TestWatchdog_TerminatingPodNeverDeleted(t *testing.T) {
	w, podTable, db, _, deleter, clock := newTestWatchdog(t, time.Minute)
	pod := runningPod("uid-3", "default", "terminating", "10.0.0.3", false)
	deletedAt := slim_metav1.Time{Time: clock.Now()}
	pod.DeletionTimestamp = &deletedAt
	insertPod(t, db, podTable, pod)

	require.NoError(t, w.check(context.Background()))
	clock.Advance(time.Hour)
	require.NoError(t, w.check(context.Background()))

	require.Empty(t, deleter.deleted)
}

// TestWatchdog_MissingStatusStartTimeIsIgnored is a direct regression test
// for a live incident: a control plane that never populates
// pod.Status.StartTime (it's nil on every pod, always) silently defeated an
// earlier StartTime-based grace period, so the watchdog never healed
// anything on that cluster. The grace period must be measured purely from
// the watchdog's own observations, never from pod.Status.StartTime.
func TestWatchdog_MissingStatusStartTimeIsIgnored(t *testing.T) {
	w, podTable, db, _, deleter, clock := newTestWatchdog(t, time.Minute)
	pod := runningPod("uid-9", "default", "no-start-time", "10.0.0.9", false)
	require.Nil(t, pod.Status.StartTime, "test fixture must reproduce a control plane that never sets StartTime")
	insertPod(t, db, podTable, pod)

	require.NoError(t, w.check(context.Background()))
	require.Empty(t, deleter.deleted, "must not heal on the first observation alone")

	clock.Advance(time.Minute)
	require.NoError(t, w.check(context.Background()))
	require.Equal(t, []string{"default/no-start-time"}, deleter.deleted,
		"must heal once the grace period elapses, even though status.StartTime was never set")
}

func TestWatchdog_NotHealedBeforeGracePeriodElapses(t *testing.T) {
	w, podTable, db, _, deleter, clock := newTestWatchdog(t, time.Minute)
	pod := runningPod("uid-4", "default", "recent", "10.0.0.4", false)
	insertPod(t, db, podTable, pod)

	require.NoError(t, w.check(context.Background()))
	require.Empty(t, deleter.deleted)

	clock.Advance(30 * time.Second) // less than the 1-minute grace period
	require.NoError(t, w.check(context.Background()))
	require.Empty(t, deleter.deleted, "must not heal before the grace period has elapsed")

	clock.Advance(31 * time.Second) // now over 1 minute since first observed
	require.NoError(t, w.check(context.Background()))
	require.Equal(t, []string{"default/recent"}, deleter.deleted)
}

func TestWatchdog_MissingEndpointHealedOnceGracePeriodElapses(t *testing.T) {
	w, podTable, db, _, deleter, clock := newTestWatchdog(t, time.Minute)
	pod := runningPod("uid-5", "default", "broken", "10.0.0.5", false)
	insertPod(t, db, podTable, pod)

	require.NoError(t, w.check(context.Background()))
	require.Empty(t, deleter.deleted, "must not heal on the first observation alone")

	clock.Advance(time.Minute)
	require.NoError(t, w.check(context.Background()))
	require.Equal(t, []string{"default/broken"}, deleter.deleted, "must heal once the condition persists past the grace period")
}

func TestWatchdog_RecoveryBetweenScansResetsPending(t *testing.T) {
	w, podTable, db, eps, deleter, clock := newTestWatchdog(t, time.Minute)
	pod := runningPod("uid-6", "default", "flaky", "10.0.0.6", false)
	insertPod(t, db, podTable, pod)

	require.NoError(t, w.check(context.Background()))
	require.Empty(t, deleter.deleted)

	// Endpoint appears before the grace period elapses: the pod recovered on its own.
	addr := netip.MustParseAddr("10.0.0.6")
	eps.byIP[addr.String()] = &endpoint.Endpoint{}
	clock.Advance(time.Minute)
	require.NoError(t, w.check(context.Background()))
	require.Empty(t, deleter.deleted)

	// Endpoint disappears again: must restart the grace period from scratch,
	// not immediately re-heal from stale pending state.
	delete(eps.byIP, addr.String())
	require.NoError(t, w.check(context.Background()))
	require.Empty(t, deleter.deleted, "must not heal immediately after a fresh reappearance of the condition")

	clock.Advance(time.Minute)
	require.NoError(t, w.check(context.Background()))
	require.Equal(t, []string{"default/flaky"}, deleter.deleted)
}

func TestWatchdog_DeleteNotFoundIsNotAnError(t *testing.T) {
	w, podTable, db, _, deleter, clock := newTestWatchdog(t, time.Minute)
	deleter.err = k8serrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "already-gone")
	pod := runningPod("uid-7", "default", "already-gone", "10.0.0.7", false)
	insertPod(t, db, podTable, pod)

	require.NoError(t, w.check(context.Background()))
	clock.Advance(time.Minute)
	require.NoError(t, w.check(context.Background()))

	require.Equal(t, []string{"default/already-gone"}, deleter.deleted)
}

func TestWatchdog_DeleteErrorDoesNotFailCheck(t *testing.T) {
	w, podTable, db, _, deleter, clock := newTestWatchdog(t, time.Minute)
	deleter.err = errors.New("boom")
	pod := runningPod("uid-8", "default", "delete-fails", "10.0.0.8", false)
	insertPod(t, db, podTable, pod)

	require.NoError(t, w.check(context.Background()))
	clock.Advance(time.Minute)
	require.NoError(t, w.check(context.Background()))

	require.Equal(t, []string{"default/delete-fails"}, deleter.deleted)
}

// The engifire regression: ownership, isolated.
//
// On 2026-09-03 the DaemonSet landed on a node where perigeos still ran the
// kube-router/bridge conflist. No pod there had a Cilium endpoint, so every
// pod-network pod matched "Running with an IP but no local endpoint" and was
// deleted; each replacement came back through the same bridge CNI and matched
// again. Four workloads plus a probe went in about three minutes, then looped.
//
// The pods were never broken. They were not ours.
//
// Both arms keep two HEALTHY pods so the blast-radius guard stays silent and
// CNI ownership is the only variable; the guard has its own test below.
func TestWatchdog_StandsDownWhenNodeUsesAnotherCNI(t *testing.T) {
	setup := func(t *testing.T, owned bool) (*watchdog, *fakePodDeleter, *fakeClock) {
		w, podTable, db, eps, deleter, clock := newTestWatchdog(t, time.Minute)
		w.cniOwned = func(context.Context) (bool, string) {
			if owned {
				return true, "test: cilium-cni"
			}
			return false, "test: node label says standard, not constellation"
		}
		for i, name := range []string{"healthy-a", "healthy-b", "inventx-front", "stz-test"} {
			ip := fmt.Sprintf("10.244.1.%d", i+1)
			insertPod(t, db, podTable,
				runningPod(k8stypes.UID(name), "default", name, ip, false))
			if strings.HasPrefix(name, "healthy") {
				eps.byIP[ip] = &endpoint.Endpoint{}
			}
		}
		return w, deleter, clock
	}

	t.Run("other CNI: deletes nothing, ever", func(t *testing.T) {
		w, deleter, clock := setup(t, false)
		for range 5 {
			require.NoError(t, w.check(context.Background()))
			clock.Advance(time.Hour)
		}
		require.Empty(t, deleter.deleted,
			"a pod that arrived through another CNI is not missing an endpoint, it is not ours")
		require.Empty(t, w.pending,
			"pods on a foreign-CNI node must not even enter the grace period")
	})

	t.Run("cilium-managed: still heals", func(t *testing.T) {
		w, deleter, clock := setup(t, true)
		require.NoError(t, w.check(context.Background()))
		clock.Advance(time.Hour)
		require.NoError(t, w.check(context.Background()))
		require.ElementsMatch(t,
			[]string{"default/inventx-front", "default/stz-test"}, deleter.deleted,
			"the gate must not disable healing on a node we do manage")
	})
}

// The guard that holds when the gate is wrong.
//
// The ownership gate has already been wrong once, on evidence that looked
// conclusive. A stale node label, an unlabelled node, or perigeos crashing
// between switching backends and updating the label all land in the same
// place. So: even with ownership asserted, "not one pod on this node has an
// endpoint" is a wrong premise, not N independently broken pods.
func TestWatchdog_BlastRadiusGuard(t *testing.T) {
	newNode := func(t *testing.T, pods int, healthy int) (*watchdog, *fakePodDeleter, *fakeClock) {
		w, podTable, db, eps, deleter, clock := newTestWatchdog(t, time.Minute)
		w.cniOwned = func(context.Context) (bool, string) { return true, "test: ours" }
		for i := range pods {
			ip := fmt.Sprintf("10.244.2.%d", i+1)
			insertPod(t, db, podTable,
				runningPod(k8stypes.UID(fmt.Sprintf("p%d", i)), "default",
					fmt.Sprintf("pod-%d", i), ip, false))
			if i < healthy {
				eps.byIP[ip] = &endpoint.Endpoint{}
			}
		}
		return w, deleter, clock
	}

	t.Run("whole node missing endpoints: refuses, repeatedly", func(t *testing.T) {
		w, deleter, clock := newNode(t, 5, 0) // engifire's shape
		for range 4 {
			require.NoError(t, w.check(context.Background()))
			clock.Advance(time.Hour)
		}
		require.Empty(t, deleter.deleted,
			"zero endpoints across the whole node is one broken assumption, not five broken pods")
	})

	t.Run("one endpoint standing: heals the rest", func(t *testing.T) {
		w, deleter, clock := newNode(t, 5, 1)
		require.NoError(t, w.check(context.Background()))
		clock.Advance(time.Hour)
		require.NoError(t, w.check(context.Background()))
		require.Len(t, deleter.deleted, 4,
			"a real post-restart endpoint loss leaves some endpoints standing; heal those that lost theirs")
	})

	t.Run("below the floor: a lone broken pod is still healed", func(t *testing.T) {
		w, deleter, clock := newNode(t, 1, 0)
		require.NoError(t, w.check(context.Background()))
		clock.Advance(time.Hour)
		require.NoError(t, w.check(context.Background()))
		require.Len(t, deleter.deleted, 1,
			"the guard must not disable the single-pod case the watchdog exists for")
	})
}

// The stand-down is a steady state on a node we do not manage, not an event,
// so it must not reprint every scan interval for the life of the agent.
func TestWatchdog_StandDownLogsOnce(t *testing.T) {
	w, _, _, _, _, clock := newTestWatchdog(t, time.Minute)
	calls := 0
	w.cniOwned = func(context.Context) (bool, string) { calls++; return false, "test: not ours" }

	for range 4 {
		require.NoError(t, w.check(context.Background()))
		clock.Advance(time.Hour)
	}
	require.Equal(t, 4, calls, "ownership is re-evaluated every scan, so a migrated node recovers")
	require.True(t, w.standDownLogged, "the message is latched after the first scan")
}

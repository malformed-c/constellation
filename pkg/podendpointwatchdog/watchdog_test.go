// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package podendpointwatchdog

import (
	"context"
	"errors"
	"net/netip"
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

func newTestWatchdog(t *testing.T, gracePeriod time.Duration) (*watchdog, statedb.RWTable[tables.LocalPod], *statedb.DB, *fakeEndpointLookup, *fakePodDeleter) {
	t.Helper()

	db := statedb.New()
	podTable, err := tables.NewPodTable(db)
	require.NoError(t, err)

	eps := &fakeEndpointLookup{byIP: map[string]*endpoint.Endpoint{}}
	deleter := &fakePodDeleter{}

	w := &watchdog{
		logger:      hivetest.Logger(t),
		db:          db,
		podTable:    podTable,
		endpoints:   eps,
		podDeleter:  deleter,
		gracePeriod: gracePeriod,
		pending:     make(map[k8stypes.UID]time.Time),
	}
	return w, podTable, db, eps, deleter
}

func insertPod(t *testing.T, db *statedb.DB, podTable statedb.RWTable[tables.LocalPod], pod tables.LocalPod) {
	t.Helper()
	txn := db.WriteTxn(podTable)
	_, _, err := podTable.Insert(txn, pod)
	require.NoError(t, err)
	txn.Commit()
}

func runningPod(uid k8stypes.UID, namespace, name, ip string, age time.Duration, hostNetwork bool) tables.LocalPod {
	startTime := slim_metav1.Time{Time: time.Now().Add(-age)}
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
			Phase:     slim_corev1.PodRunning,
			PodIP:     ip,
			StartTime: &startTime,
		},
	}}
}

func TestWatchdog_HealthyPodNeverDeleted(t *testing.T) {
	w, podTable, db, eps, deleter := newTestWatchdog(t, time.Minute)
	pod := runningPod("uid-1", "default", "healthy", "10.0.0.1", 10*time.Minute, false)
	insertPod(t, db, podTable, pod)

	addr := netip.MustParseAddr("10.0.0.1")
	eps.byIP[addr.String()] = &endpoint.Endpoint{}

	require.NoError(t, w.check(context.Background()))
	require.NoError(t, w.check(context.Background()))

	require.Empty(t, deleter.deleted)
}

func TestWatchdog_HostNetworkPodNeverDeleted(t *testing.T) {
	w, podTable, db, _, deleter := newTestWatchdog(t, time.Minute)
	pod := runningPod("uid-2", "kube-system", "host-net", "10.0.0.2", 10*time.Minute, true)
	insertPod(t, db, podTable, pod)

	require.NoError(t, w.check(context.Background()))
	require.NoError(t, w.check(context.Background()))

	require.Empty(t, deleter.deleted, "hostNetwork pods never get their own Cilium endpoint and must never be healed")
}

func TestWatchdog_TerminatingPodNeverDeleted(t *testing.T) {
	w, podTable, db, _, deleter := newTestWatchdog(t, time.Minute)
	pod := runningPod("uid-3", "default", "terminating", "10.0.0.3", 10*time.Minute, false)
	now := slim_metav1.Time{Time: time.Now()}
	pod.DeletionTimestamp = &now
	insertPod(t, db, podTable, pod)

	require.NoError(t, w.check(context.Background()))
	require.NoError(t, w.check(context.Background()))

	require.Empty(t, deleter.deleted)
}

func TestWatchdog_TooYoungPodNotHealedYet(t *testing.T) {
	w, podTable, db, _, deleter := newTestWatchdog(t, time.Minute)
	pod := runningPod("uid-4", "default", "fresh", "10.0.0.4", 5*time.Second, false)
	insertPod(t, db, podTable, pod)

	require.NoError(t, w.check(context.Background()))
	require.NoError(t, w.check(context.Background()))

	require.Empty(t, deleter.deleted, "a pod younger than the grace period must not be healed even if repeatedly missing")
}

func TestWatchdog_MissingEndpointHealedOnSecondConsecutiveScan(t *testing.T) {
	w, podTable, db, _, deleter := newTestWatchdog(t, time.Minute)
	pod := runningPod("uid-5", "default", "broken", "10.0.0.5", 10*time.Minute, false)
	insertPod(t, db, podTable, pod)

	require.NoError(t, w.check(context.Background()))
	require.Empty(t, deleter.deleted, "must not heal on the first observation alone")

	require.NoError(t, w.check(context.Background()))
	require.Equal(t, []string{"default/broken"}, deleter.deleted, "must heal once the condition persists across a second scan")
}

func TestWatchdog_RecoveryBetweenScansResetsPending(t *testing.T) {
	w, podTable, db, eps, deleter := newTestWatchdog(t, time.Minute)
	pod := runningPod("uid-6", "default", "flaky", "10.0.0.6", 10*time.Minute, false)
	insertPod(t, db, podTable, pod)

	require.NoError(t, w.check(context.Background()))
	require.Empty(t, deleter.deleted)

	// Endpoint appears before the second scan: the pod recovered on its own.
	addr := netip.MustParseAddr("10.0.0.6")
	eps.byIP[addr.String()] = &endpoint.Endpoint{}
	require.NoError(t, w.check(context.Background()))
	require.Empty(t, deleter.deleted)

	// Endpoint disappears again: must restart the two-scan confirmation,
	// not immediately re-heal from stale pending state.
	delete(eps.byIP, addr.String())
	require.NoError(t, w.check(context.Background()))
	require.Empty(t, deleter.deleted, "must not heal immediately after a fresh reappearance of the condition")

	require.NoError(t, w.check(context.Background()))
	require.Equal(t, []string{"default/flaky"}, deleter.deleted)
}

func TestWatchdog_DeleteNotFoundIsNotAnError(t *testing.T) {
	w, podTable, db, _, deleter := newTestWatchdog(t, time.Minute)
	deleter.err = k8serrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "already-gone")
	pod := runningPod("uid-7", "default", "already-gone", "10.0.0.7", 10*time.Minute, false)
	insertPod(t, db, podTable, pod)

	require.NoError(t, w.check(context.Background()))
	require.NoError(t, w.check(context.Background()))

	require.Equal(t, []string{"default/already-gone"}, deleter.deleted)
}

func TestWatchdog_DeleteErrorDoesNotFailCheck(t *testing.T) {
	w, podTable, db, _, deleter := newTestWatchdog(t, time.Minute)
	deleter.err = errors.New("boom")
	pod := runningPod("uid-8", "default", "delete-fails", "10.0.0.8", 10*time.Minute, false)
	insertPod(t, db, podTable, pod)

	require.NoError(t, w.check(context.Background()))
	require.NoError(t, w.check(context.Background()))

	require.Equal(t, []string{"default/delete-fails"}, deleter.deleted)
}

// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Constellation

package tables

// Tests for initial managed-node discovery.
//
// Discovery runs during hive object-graph population, before the k8s client
// cell's start hook has run upstream's connection wait/failover loop. On a cold
// boot where the API server (a static pod) comes up after the agent, discovery
// is therefore the first code to touch it, and an un-retried error surfaces as
// "failed to populate object graph" — a hard fatal on every cold boot.

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cilium/hive/hivetest"
	"github.com/stretchr/testify/require"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sTesting "k8s.io/client-go/testing"

	k8sclient "github.com/cilium/cilium/pkg/k8s/client"
	k8sTestUtils "github.com/cilium/cilium/pkg/k8s/client/testutils"
	slim_corev1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/core/v1"
	slim_metav1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/apis/meta/v1"
	nodeTypes "github.com/cilium/cilium/pkg/node/types"
)

// errUnreachable has the shape of a genuinely unreachable endpoint: a plain
// transport error, not an APIStatus the server chose to return.
var errUnreachable = errors.New("dial tcp 127.0.0.1:6443: connect: connection refused")

// shortenDiscoveryRetries makes the retry loop test-speed, and afterwards
// restores the production values and clears the managed-name list that
// discovery sets as a side effect.
//
// The list is cleared rather than snapshot-and-restored: unset is not the same
// as "set to whatever GetManagedNames() defaulted to", because that default is
// [GetName()] evaluated lazily. Writing the snapshot back would freeze this
// process's node name into the global and break any later test that sets its
// own node name — which is exactly what it did to TestScript.
func shortenDiscoveryRetries(t *testing.T, timeout, interval time.Duration) {
	t.Helper()
	origTimeout, origInterval := managedNodeDiscoveryTimeout, managedNodeDiscoveryInterval
	managedNodeDiscoveryTimeout, managedNodeDiscoveryInterval = timeout, interval
	t.Cleanup(func() {
		managedNodeDiscoveryTimeout, managedNodeDiscoveryInterval = origTimeout, origInterval
		nodeTypes.SetManagedNames(nil)
	})
}

// failNodeListTimes makes the next n Node List calls fail with err, after which
// the fake's normal object tracker serves the request. Returns the call counter.
func failNodeListTimes(cs *k8sTestUtils.FakeClientset, n int32, err error) *atomic.Int32 {
	var calls atomic.Int32
	cs.SlimFakeClientset.PrependReactor("list", "nodes",
		func(k8sTesting.Action) (bool, runtime.Object, error) {
			if calls.Add(1) <= n {
				return true, nil, err
			}
			return false, nil, nil
		})
	return &calls
}

func newDiscoveryFake(t *testing.T) (*k8sTestUtils.FakeClientset, k8sclient.Clientset) {
	t.Helper()
	return k8sTestUtils.NewFakeClientset(hivetest.Logger(t))
}

// The API server is unreachable for the first few attempts, as on a cold boot
// where it starts after the agent. Discovery must retry and then succeed —
// against the un-retried version this fails on the very first attempt.
func TestDiscoverManagedNodes_RetriesUntilAPIServerAnswers(t *testing.T) {
	shortenDiscoveryRetries(t, 10*time.Second, time.Millisecond)

	fake, cs := newDiscoveryFake(t)
	require.NoError(t, fake.SlimFakeClientset.Tracker().Add(&slim_corev1.Node{
		ObjectMeta: slim_metav1.ObjectMeta{
			Name:   "pawn-a",
			Labels: map[string]string{"peri.apsis/host": "engix99"},
		},
	}))
	calls := failNodeListTimes(fake, 3, errUnreachable)

	names, err := discoverManagedNodesWithRetry(context.Background(), cs, "peri.apsis/host")
	require.NoError(t, err)
	require.Equal(t, []string{"pawn-a"}, names)
	require.EqualValues(t, 4, calls.Load(), "should have failed three times then succeeded")
	require.Equal(t, []string{"pawn-a"}, nodeTypes.GetManagedNames())
}

// A permanently unreachable API server must still fail — bounded, not hung —
// so the operator sees the real cause instead of a silently stalled startup.
func TestDiscoverManagedNodes_GivesUpAfterTimeout(t *testing.T) {
	shortenDiscoveryRetries(t, 50*time.Millisecond, time.Millisecond)

	fake, cs := newDiscoveryFake(t)
	failNodeListTimes(fake, math.MaxInt32, errUnreachable)

	start := time.Now()
	_, err := discoverManagedNodesWithRetry(context.Background(), cs, "peri.apsis/host")
	require.Error(t, err)
	require.ErrorContains(t, err, "gave up after")
	require.Less(t, time.Since(start), 5*time.Second, "must not hang past its budget")
}

// Errors caused by the request rather than by the connection will not fix
// themselves; retrying them for minutes would only hide the real cause.
func TestDiscoverManagedNodes_FailsFastOnTerminalErrors(t *testing.T) {
	// A long budget: were these retried, the test would block on it.
	shortenDiscoveryRetries(t, time.Minute, time.Second)

	for name, apiErr := range map[string]error{
		"forbidden":   k8serrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("denied")),
		"bad request": k8serrors.NewBadRequest("unable to parse requirement"),
	} {
		t.Run(name, func(t *testing.T) {
			fake, cs := newDiscoveryFake(t)
			calls := failNodeListTimes(fake, math.MaxInt32, apiErr)

			start := time.Now()
			_, err := discoverManagedNodesWithRetry(context.Background(), cs, "peri.apsis/host")
			require.Error(t, err)
			require.EqualValues(t, 1, calls.Load(), "must not retry a terminal error")
			require.Less(t, time.Since(start), 5*time.Second)
		})
	}
}

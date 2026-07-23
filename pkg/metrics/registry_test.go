// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package metrics_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/cilium/hive"
	"github.com/cilium/hive/cell"
	"github.com/cilium/hive/hivetest"
	"github.com/cilium/hive/job"
	"github.com/stretchr/testify/require"

	"github.com/cilium/cilium/pkg/metrics"
)

// fakeShutdowner records Shutdown() calls instead of tearing down a real
// hive, so tests can assert whether a listen/serve failure triggered a
// hive-wide shutdown without needing to race the real shutdown machinery.
type fakeShutdowner struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeShutdowner) Shutdown(...hive.ShutdownOption) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
}

func (f *fakeShutdowner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newTestRegistry builds a Registry wired to a real job.Group (via a
// minimal hive carrying only job.Cell) and a fakeShutdowner, and calls
// AddServerRuntimeHooks with the given address before starting the hive so
// the OneShot job is queued rather than run immediately.
func newTestRegistry(t *testing.T, addr string) (*metrics.Registry, *fakeShutdowner, *hive.Hive) {
	t.Helper()
	log := hivetest.Logger(t)
	shutdowner := &fakeShutdowner{}

	health, _ := cell.NewSimpleHealth()
	var group job.Group
	var lc cell.Lifecycle
	h := hive.New(
		job.Cell,
		cell.Invoke(func(jr job.Registry, lifecycle cell.Lifecycle) {
			group = jr.NewGroup(health)
			lc = lifecycle
		}),
	)
	require.NoError(t, h.Populate(hivetest.Logger(t)))

	reg := metrics.NewRegistry(metrics.RegistryParams{
		Logger:     log,
		Shutdowner: shutdowner,
		Lifecycle:  lc,
		JobGroup:   group,
		Config:     metrics.RegistryConfig{PrometheusServeAddr: addr},
	})

	return reg, shutdowner, h
}

// TestAddServerRuntimeHooksListenFailureDoesNotShutdownHive is a
// regression test: previously, a failure to bind the prometheus metrics
// address (e.g. EADDRINUSE from a stale process) called
// Shutdowner.Shutdown(), taking down the whole agent/operator for a
// non-critical metrics endpoint. The error must now just be logged.
func TestAddServerRuntimeHooksListenFailureDoesNotShutdownHive(t *testing.T) {
	// Occupy the port first so the registry's own Listen() call fails.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer blocker.Close()
	addr := blocker.Addr().String()

	reg, shutdowner, h := newTestRegistry(t, addr)
	reg.AddServerRuntimeHooks("test-prometheus-server", nil, net.ListenConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, h.Start(hivetest.Logger(t), ctx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		require.NoError(t, h.Stop(hivetest.Logger(t), stopCtx))
	})

	require.Eventually(t, func() bool {
		return shutdowner.callCount() == 0
	}, 2*time.Second, 20*time.Millisecond,
		"a listen failure must not call Shutdowner.Shutdown()")

	// Give the OneShot's error path a further moment to prove the
	// negative isn't just a timing artifact of Eventually's first pass.
	time.Sleep(200 * time.Millisecond)
	require.Zero(t, shutdowner.callCount())
}

// TestAddServerRuntimeHooksServesMetrics is a baseline check that the
// happy path still works: the server binds, serves /metrics, and never
// calls Shutdowner.Shutdown().
func TestAddServerRuntimeHooksServesMetrics(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	reg, shutdowner, h := newTestRegistry(t, addr)
	reg.AddServerRuntimeHooks("test-prometheus-server", nil, net.ListenConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, h.Start(hivetest.Logger(t), ctx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		require.NoError(t, h.Stop(hivetest.Logger(t), stopCtx))
	})

	var resp *http.Response
	require.Eventually(t, func() bool {
		var getErr error
		resp, getErr = http.Get(fmt.Sprintf("http://%s/metrics", addr))
		return getErr == nil
	}, 2*time.Second, 20*time.Millisecond, "metrics server never became reachable")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Zero(t, shutdowner.callCount())
}

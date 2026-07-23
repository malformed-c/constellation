// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package metrics_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/cilium/hive"
	"github.com/cilium/hive/cell"
	"github.com/cilium/hive/hivetest"
	"github.com/cilium/hive/job"
	"github.com/stretchr/testify/require"

	"github.com/cilium/cilium/pkg/metrics"
)

// newTestHive builds a minimal hive that constructs a Registry and calls
// AddServerRuntimeHooks with the given address as part of an Invoke, so the
// OneShot job is queued as soon as the hive starts.
func newTestHive(t *testing.T, addr string) *hive.Hive {
	t.Helper()
	log := hivetest.Logger(t)
	return hive.New(
		job.Cell,
		cell.Invoke(func(jr job.Registry, lifecycle cell.Lifecycle) {
			health, _ := cell.NewSimpleHealth()
			group := jr.NewGroup(health)
			reg := metrics.NewRegistry(metrics.RegistryParams{
				Logger:    log,
				Lifecycle: lifecycle,
				JobGroup:  group,
				Config:    metrics.RegistryConfig{PrometheusServeAddr: addr},
			})
			reg.AddServerRuntimeHooks("test-prometheus-server", nil, net.ListenConfig{})
		}),
	)
}

// TestAddServerRuntimeHooksListenFailureDoesNotShutdownHive is a
// regression test: previously, a failure to bind the prometheus metrics
// address (e.g. EADDRINUSE from a stale process) shut down the whole
// hive for a non-critical metrics endpoint. The OneShot job must now just
// return the error - job.WithShutdown() propagates a start-hook failure
// through Run() without the job itself killing the process - and the hive
// as a whole must not self-terminate. Races Run() against a timeout:
// self-termination would make Run() return well within it.
func TestAddServerRuntimeHooksListenFailureDoesNotShutdownHive(t *testing.T) {
	// Occupy the port first so the registry's own Listen() call fails.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer blocker.Close()
	addr := blocker.Addr().String()

	h := newTestHive(t, addr)

	runDone := make(chan error, 1)
	go func() { runDone <- h.Run(hivetest.Logger(t)) }()

	select {
	case err := <-runDone:
		t.Fatalf("hive self-terminated from a listen failure (err=%v) - it must stay up", err)
	case <-time.After(2 * time.Second):
		// Still running after 2s: the listen failure did not bring the
		// hive down. Clean up ourselves since Run() never returned.
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	h.Shutdown()
	select {
	case <-runDone:
	case <-stopCtx.Done():
		t.Fatal("hive did not stop after Shutdown()")
	}
}

// TestAddServerRuntimeHooksServesMetrics is a baseline check that the
// happy path still works: the server binds and serves /metrics.
func TestAddServerRuntimeHooksServesMetrics(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	h := newTestHive(t, addr)

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
}

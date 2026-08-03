// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cilium/hive/cell"
	"github.com/cilium/hive/hivetest"
	"github.com/cilium/statedb"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/cilium/cilium/pkg/hive"
	k8smetrics "github.com/cilium/cilium/pkg/k8s/metrics"
	k8sversion "github.com/cilium/cilium/pkg/k8s/version"
	"github.com/cilium/cilium/pkg/loadbalancer"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/testutils"
)

func Test_runHeartbeat(t *testing.T) {
	// k8s api server never replied back in the expected time. We should close all connections
	k8smetrics.LastSuccessInteraction.Reset()
	time.Sleep(2 * time.Millisecond)

	testCtx, testCtxCancel := context.WithCancel(context.Background())

	called := make(chan struct{})
	runHeartbeat(
		hivetest.Logger(t),
		func(ctx context.Context) error {
			// Block any attempt to connect return from a heartbeat until the
			// test is complete.
			<-testCtx.Done()
			return nil
		},
		time.Millisecond,
		func() {
			close(called)
		},
	)

	// We need to polling for the condition instead of using a time.After to
	// give the opportunity for scheduler to run the goroutine inside runHeartbeat
	err := testutils.WaitUntil(func() bool {
		select {
		case <-called:
			return true
		default:
			return false
		}
	},
		5*time.Second)
	require.NoError(t, err, "Heartbeat should have closed all connections")
	testCtxCancel()

	// There are some connectivity issues, cilium is trying to reach kube-apiserver
	// but it's only receiving errors for other requests. We should close all
	// connections!

	// Wait the double amount of time than the timeout to make sure
	// LastSuccessInteraction is not taken into account and we will see that we
	// will close all connections.
	testCtx, testCtxCancel = context.WithCancel(context.Background())
	time.Sleep(20 * time.Millisecond)

	called = make(chan struct{})
	runHeartbeat(
		hivetest.Logger(t),
		func(ctx context.Context) error {
			// Block any attempt to connect return from a heartbeat until the
			// test is complete.
			<-testCtx.Done()
			return nil
		},
		10*time.Millisecond,
		func() {
			close(called)
		},
	)

	// We need to polling for the condition instead of using a time.After to
	// give the opportunity for scheduler to run the goroutine inside runHeartbeat
	err = testutils.WaitUntil(func() bool {
		select {
		case <-called:
			return true
		default:
			return false
		}
	},
		5*time.Second)
	require.NoError(t, err, "Heartbeat should have closed all connections")
	testCtxCancel()

	// Cilium is successfully talking with kube-apiserver, we should not do
	// anything.
	k8smetrics.LastSuccessInteraction.Reset()

	called = make(chan struct{})
	runHeartbeat(
		hivetest.Logger(t),
		func(ctx context.Context) error {
			close(called)
			return nil
		},
		10*time.Millisecond,
		func() {
			t.Error("This should not have been called!")
		},
	)

	select {
	case <-time.After(20 * time.Millisecond):
	case <-called:
		t.Error("Heartbeat should have closed all connections")
	}

	// Cilium had the last interaction with kube-apiserver a long time ago.
	// We should perform a heartbeat
	k8smetrics.LastInteraction.Reset()
	time.Sleep(50 * time.Millisecond)

	called = make(chan struct{})
	runHeartbeat(
		hivetest.Logger(t),
		func(ctx context.Context) error {
			close(called)
			return nil
		},
		10*time.Millisecond,
		func() {
			t.Error("This should not have been called!")
		},
	)

	// We need to polling for the condition instead of using a time.After to
	// give the opportunity for scheduler to run the goroutine inside runHeartbeat
	err = testutils.WaitUntil(func() bool {
		select {
		case <-called:
			return true
		default:
			return false
		}
	},
		5*time.Second)
	require.NoError(t, err, "Heartbeat should have closed all connections")

	// Cilium had the last interaction with kube-apiserver a long time ago.
	// We should perform a heartbeat but the heart beat will return
	// an error so we should close all connections
	k8smetrics.LastInteraction.Reset()
	time.Sleep(50 * time.Millisecond)

	called = make(chan struct{})
	runHeartbeat(
		hivetest.Logger(t),
		func(ctx context.Context) error {
			return &errors.StatusError{
				ErrStatus: metav1.Status{
					Code: http.StatusRequestTimeout,
				},
			}
		},
		10*time.Millisecond,
		func() {
			close(called)
		},
	)

	// We need to polling for the condition instead of using a time.After to
	// give the opportunity for scheduler to run the goroutine inside runHeartbeat
	err = testutils.WaitUntil(func() bool {
		select {
		case <-called:
			return true
		default:
			return false
		}
	},
		5*time.Second)
	require.NoError(t, err, "Heartbeat should have closed all connections")
}

func Test_client(t *testing.T) {
	var requests lock.Map[string, *http.Request]
	getRequest := func(k string) *http.Request {
		v, _ := requests.Load(k)
		return v
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Store(r.URL.Path, r)

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			w.Write([]byte(`{
			       "major": "1",
			       "minor": "99"
			}`))
		default:
			w.Write([]byte("{}"))
		}
	}))
	srv.Start()
	defer srv.Close()

	var clientset Clientset
	hive := hive.New(
		Cell,
		cell.Provide(
			loadbalancer.NewFrontendsTable, statedb.RWTable[*loadbalancer.Frontend].ToTable,
			func() loadbalancer.Config { return loadbalancer.DefaultConfig },
		),
		cell.Invoke(func(c Clientset) { clientset = c }),
	)

	// Set the server URL and use a low heartbeat timeout for quick test completion.
	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	hive.RegisterFlags(flags)
	flags.Set(option.K8sAPIServerURLs, srv.URL)
	flags.Set(option.K8sHeartbeatTimeout, "150ms")
	// Set a higher QPS limit as the test exercises timing aspects.
	flags.Set(option.K8sClientQPSLimit, "500")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	tlog := hivetest.Logger(t)
	require.NoError(t, hive.Start(tlog, ctx))

	// Check that we see the connection probe and version check
	require.NotNil(t, getRequest("/api/v1/namespaces/kube-system"))
	require.NotNil(t, getRequest("/version"))
	semVer := k8sversion.Version()
	require.Equal(t, uint64(99), semVer.Minor)

	// Wait until heartbeat has been seen to check that heartbeats are
	// running.
	err := testutils.WaitUntil(
		func() bool { return getRequest("/readyz") != nil },
		time.Second)
	require.NoError(t, err)

	// Test that all different clientsets are wired correctly.
	_, err = clientset.CoreV1().Pods("test").Get(context.TODO(), "pod", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, getRequest("/api/v1/namespaces/test/pods/pod"))

	_, err = clientset.Slim().CoreV1().Pods("test").Get(context.TODO(), "slim-pod", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, getRequest("/api/v1/namespaces/test/pods/slim-pod"))

	_, err = clientset.ExtensionsV1beta1().DaemonSets("test").Get(context.TODO(), "ds", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, getRequest("/apis/extensions/v1beta1/namespaces/test/daemonsets/ds"))

	_, err = clientset.CiliumV2().CiliumEndpoints("test").Get(context.TODO(), "ces", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, getRequest("/apis/cilium.io/v2/namespaces/test/ciliumendpoints/ces"))

	require.NoError(t, hive.Stop(tlog, ctx))
}

func Test_clientMultipleAPIServers(t *testing.T) {
	var requests lock.Map[string, *http.Request]
	getRequest := func(k string) *http.Request {
		v, _ := requests.Load(k)
		return v
	}
	apiStateFile, err := os.CreateTemp("", "kubeapi_state")
	require.NoError(t, err)
	K8sAPIServerFilePath = apiStateFile.Name()

	servers := make([]*httptest.Server, 3)
	for i := range 3 {
		servers[i] = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Store(r.URL.Path, r)

			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/version":
				w.Write([]byte(`{
			       "major": "1",
			       "minor": "99"
			}`))
			default:
				w.Write([]byte("{}"))
			}
		}))
	}
	servers[0].Start()
	defer servers[0].Close()
	servers[1].Start()
	servers[2].Start()

	var clientset Clientset
	hive := hive.New(
		Cell,
		cell.Provide(
			loadbalancer.NewFrontendsTable, statedb.RWTable[*loadbalancer.Frontend].ToTable,
			func() loadbalancer.Config { return loadbalancer.DefaultConfig },
		),
		cell.Invoke(func(c Clientset) { clientset = c }),
	)

	// Set the server URL and use a low heartbeat timeout for quick test completion.
	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	hive.RegisterFlags(flags)
	urls := []string{servers[0].URL, servers[1].URL, servers[2].URL}
	flags.Set(option.K8sAPIServerURLs, strings.Join(urls, ","))
	flags.Set(option.K8sHeartbeatTimeout, "150ms")
	// Set a higher QPS limit as the test exercises timing aspects.
	flags.Set(option.K8sClientQPSLimit, "500")
	// 2/3 servers are stopped in order to validate that the agent connects to an active server.
	servers[1].Close()
	servers[2].Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	tlog := hivetest.Logger(t)
	require.NoError(t, hive.Start(tlog, ctx))

	// Check that we see the connection probe and version check
	require.NotNil(t, getRequest("/api/v1/namespaces/kube-system"))
	require.NotNil(t, getRequest("/version"))
	semVer := k8sversion.Version()
	require.Equal(t, uint64(99), semVer.Minor)

	// Test that all different clientsets are wired correctly.
	_, err = clientset.CoreV1().Pods("test").Get(context.TODO(), "pod", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, getRequest("/api/v1/namespaces/test/pods/pod"))

	_, err = clientset.Slim().CoreV1().Pods("test").Get(context.TODO(), "slim-pod", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, getRequest("/api/v1/namespaces/test/pods/slim-pod"))

	_, err = clientset.ExtensionsV1beta1().DaemonSets("test").Get(context.TODO(), "ds", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, getRequest("/apis/extensions/v1beta1/namespaces/test/daemonsets/ds"))

	_, err = clientset.CiliumV2().CiliumEndpoints("test").Get(context.TODO(), "ces", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, getRequest("/apis/cilium.io/v2/namespaces/test/ciliumendpoints/ces"))

	require.NoError(t, hive.Stop(tlog, ctx))
}

func Test_clientMultipleAPIServersServiceSwitchover(t *testing.T) {
	var requests lock.Map[string, *http.Request]
	getRequest := func(k string) *http.Request {
		v, _ := requests.Load(k)
		return v
	}
	apiStateFile, err := os.CreateTemp("", "kubeapi_state")
	require.NoError(t, err)
	defer apiStateFile.Close()
	K8sAPIServerFilePath = apiStateFile.Name()

	servers := make([]*httptest.Server, 3)
	for i := range servers {
		servers[i] = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Store(r.URL.Path, r)

			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/version":
				w.Write([]byte(`{
			       "major": "1",
			       "minor": "99"
			}`))
			default:
				w.Write([]byte("{}"))
			}
		}))
	}
	servers[0].Start()
	servers[1].Start()

	var (
		clientset Clientset
		mgr       *restConfigManager
	)
	h := hive.New(
		Cell,
		cell.Provide(
			loadbalancer.NewFrontendsTable, statedb.RWTable[*loadbalancer.Frontend].ToTable,
			func() loadbalancer.Config { return loadbalancer.DefaultConfig },
		),
		cell.Invoke(func(c Clientset, m *restConfigManager) { clientset = c; mgr = m }),
	)

	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	h.RegisterFlags(flags)
	urls := []string{servers[0].URL, servers[1].URL}
	flags.Set(option.K8sAPIServerURLs, strings.Join(urls, ","))

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	tlog := hivetest.Logger(t)
	require.NoError(t, h.Start(tlog, ctx))

	// Check that we see the connection probe and version check
	require.NotNil(t, getRequest("/api/v1/namespaces/kube-system"))
	require.NotNil(t, getRequest("/version"))
	semVer := k8sversion.Version()
	require.Equal(t, uint64(99), semVer.Minor)

	// Start server that responds to kube-api service address.
	servers[2].Start()
	defer servers[2].Close()
	mapping := K8sServiceEndpointMapping{
		Service: servers[2].URL,
	}
	mgr.updateMappings(mapping)
	// All servers are stopped in order to validate that the agent fails over correctly.
	servers[0].Close()
	servers[1].Close()

	require.NoError(t, testutils.WaitUntil(func() bool {
		_, err = clientset.CoreV1().Pods("test").Get(context.TODO(), "pod", metav1.GetOptions{})

		return err == nil
	}, 5*time.Second))
	require.NotNil(t, getRequest("/api/v1/namespaces/test/pods/pod"))
	// Test that all different clientsets continue to have connectivity to kube-apiserver.

	_, err = clientset.Slim().CoreV1().Pods("test").Get(context.TODO(), "slim-pod", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, getRequest("/api/v1/namespaces/test/pods/slim-pod"))

	_, err = clientset.ExtensionsV1beta1().DaemonSets("test").Get(context.TODO(), "ds", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, getRequest("/apis/extensions/v1beta1/namespaces/test/daemonsets/ds"))

	_, err = clientset.CiliumV2().CiliumEndpoints("test").Get(context.TODO(), "ces", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, getRequest("/apis/cilium.io/v2/namespaces/test/ciliumendpoints/ces"))

	require.NoError(t, h.Stop(tlog, ctx))

	// Test the agent connects to the restored service address after restart.
	h = hive.New(
		Cell,
		cell.Provide(
			loadbalancer.NewFrontendsTable, statedb.RWTable[*loadbalancer.Frontend].ToTable,
			func() loadbalancer.Config { return loadbalancer.DefaultConfig },
		),
		cell.Invoke(func(c Clientset) { clientset = c }),
	)

	flags = pflag.NewFlagSet("", pflag.ContinueOnError)
	h.RegisterFlags(flags)
	flags.Set(option.K8sAPIServerURLs, strings.Join(urls, ","))
	ctx, cancel = context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	tlog = hivetest.Logger(t)
	require.NoError(t, h.Start(tlog, ctx))

	require.NoError(t, testutils.WaitUntil(func() bool {
		_, err = clientset.CoreV1().Pods("test").Get(context.TODO(), "pod", metav1.GetOptions{})

		return err == nil
	}, 5*time.Second))
	require.NotNil(t, getRequest("/api/v1/namespaces/test/pods/pod"))

	require.NoError(t, h.Stop(tlog, ctx))
}

func Test_clientMultipleAPIServersFailedRestore(t *testing.T) {
	var requests lock.Map[string, *http.Request]
	getRequest := func(k string) *http.Request {
		v, _ := requests.Load(k)
		return v
	}
	apiStateFile, err := os.CreateTemp("", "kubeapi_state")
	require.NoError(t, err)
	defer apiStateFile.Close()
	K8sAPIServerFilePath = apiStateFile.Name()

	servers := make([]*httptest.Server, 4)
	for i := range servers {
		servers[i] = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Store(r.URL.Path, r)

			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/version":
				w.Write([]byte(`{
			       "major": "1",
			       "minor": "99"
			}`))
			default:
				w.Write([]byte("{}"))
			}
		}))
	}
	servers[0].Start()
	servers[1].Start()

	var (
		clientset Clientset
		mgr       *restConfigManager
	)
	h := hive.New(
		Cell,
		cell.Provide(
			loadbalancer.NewFrontendsTable, statedb.RWTable[*loadbalancer.Frontend].ToTable,
			func() loadbalancer.Config { return loadbalancer.DefaultConfig },
		),
		cell.Invoke(func(c Clientset, m *restConfigManager) { clientset = c; mgr = m }),
	)

	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	h.RegisterFlags(flags)
	urls := []string{servers[0].URL, servers[1].URL}
	flags.Set(option.K8sAPIServerURLs, strings.Join(urls, ","))

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	tlog := hivetest.Logger(t)
	require.NoError(t, h.Start(tlog, ctx))

	// Check that we see the connection probe and version check
	require.NotNil(t, getRequest("/api/v1/namespaces/kube-system"))
	require.NotNil(t, getRequest("/version"))
	semVer := k8sversion.Version()
	require.Equal(t, uint64(99), semVer.Minor)

	// Write a bogus service address so that it won't be restored, and agent falls back
	// to user provided server URLs.
	mapping := K8sServiceEndpointMapping{
		Service: "http://0.0.0.0",
	}
	// Close previous servers, and start a new one.
	servers[0].Close()
	servers[1].Close()
	servers[2].Start()
	defer servers[2].Close()
	servers[3].Start()
	defer servers[3].Close()
	mgr.saveMapping(mapping)

	h = hive.New(
		Cell,
		cell.Provide(
			loadbalancer.NewFrontendsTable, statedb.RWTable[*loadbalancer.Frontend].ToTable,
			func() loadbalancer.Config { return loadbalancer.DefaultConfig },
		),
		cell.Invoke(func(c Clientset) { clientset = c }),
	)

	flags = pflag.NewFlagSet("", pflag.ContinueOnError)
	h.RegisterFlags(flags)
	// User provides a new server.
	urls = []string{servers[2].URL, servers[3].URL}
	flags.Set(option.K8sAPIServerURLs, strings.Join(urls, ","))
	// Set lower timeouts for tests.
	connRetryInterval = 5 * time.Millisecond
	connTimeout = 100 * time.Millisecond
	ctx, cancel = context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	tlog = hivetest.Logger(t)
	require.NoError(t, h.Start(tlog, ctx))

	require.NoError(t, testutils.WaitUntil(func() bool {
		_, err = clientset.CoreV1().Pods("test").Get(context.TODO(), "pod", metav1.GetOptions{})

		return err == nil
	}, 5*time.Second))
	require.NotNil(t, getRequest("/api/v1/namespaces/test/pods/pod"))

	require.NoError(t, h.Stop(tlog, ctx))
}

func Test_clientMultipleAPIServersFailedHeartbeat(t *testing.T) {
	var healthServer lock.Map[string, string]
	getServer := func(k string) string {
		v, _ := healthServer.Load(k)
		return v
	}
	var requests lock.Map[string, *http.Request]
	getRequest := func(k string) *http.Request {
		v, _ := requests.Load(k)
		return v
	}
	apiStateFile, err := os.CreateTemp("", "kubeapi_state")
	require.NoError(t, err)
	defer apiStateFile.Close()
	K8sAPIServerFilePath = apiStateFile.Name()

	servers := make([]*httptest.Server, 3)
	for i := range servers {
		servers[i] = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Store(r.URL.Path, r)

			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/version":
				w.Write([]byte(`{
			       "major": "1",
			       "minor": "99"
			}`))
			case "/readyz":
				healthServer.Store("health", "http://"+r.Host)
			default:
				w.Write([]byte("{}"))
			}
		}))
	}
	servers[0].Start()
	servers[1].Start()

	var (
		clientset Clientset
		mgr       *restConfigManager
	)
	h := hive.New(
		Cell,
		cell.Provide(
			loadbalancer.NewFrontendsTable, statedb.RWTable[*loadbalancer.Frontend].ToTable,
			func() loadbalancer.Config { return loadbalancer.DefaultConfig },
		),
		cell.Invoke(func(c Clientset, m *restConfigManager) { clientset = c; mgr = m }),
	)

	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	h.RegisterFlags(flags)
	urls := []string{servers[0].URL, servers[1].URL}
	flags.Set(option.K8sAPIServerURLs, strings.Join(urls, ","))
	flags.Set(option.K8sHeartbeatTimeout, "1s")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	tlog := hivetest.Logger(t)
	require.NoError(t, h.Start(tlog, ctx))

	// Check that we see the connection probe and version check
	require.NotNil(t, getRequest("/api/v1/namespaces/kube-system"))
	require.NotNil(t, getRequest("/version"))
	semVer := k8sversion.Version()
	require.Equal(t, uint64(99), semVer.Minor)

	// Fail the heartbeat to validate that API server URL is rotated.
	require.NoError(t, testutils.WaitUntil(func() bool {
		s := getServer("health")
		if s != "" {
			if servers[0].URL == s {
				// Close the current active server.
				servers[0].Close()
			} else {
				servers[1].Close()
			}
			return true
		}

		return false
	}, 5*time.Second))

	require.NoError(t, testutils.WaitUntil(func() bool {
		_, err := clientset.CoreV1().Pods("test").Get(context.TODO(), "pod", metav1.GetOptions{})

		return err == nil
	}, 5*time.Second))
	require.NotNil(t, getRequest("/api/v1/namespaces/test/pods/pod"))

	// Validate manual URL rotation isn't triggered after switch to the service address.
	// Start server that responds to kube-apiserver address.
	servers[2].Start()
	defer servers[2].Close()
	servers[0].Close()
	servers[1].Close()
	mapping := K8sServiceEndpointMapping{
		Service: servers[2].URL,
		// Add bogus endpoints
		Endpoints: []string{"10.0.0.0:60"},
	}
	mgr.updateMappings(mapping)

	require.NoError(t, testutils.WaitUntil(func() bool {
		_, err = clientset.CoreV1().Pods("test").Get(context.TODO(), "pod", metav1.GetOptions{})

		return err == nil
	}, 5*time.Second))
	require.NotNil(t, getRequest("/api/v1/namespaces/test/pods/pod"))

	require.NoError(t, h.Stop(tlog, ctx))
}

// Test_clientMultipleAPIServersServiceHeartbeatFallback is a regression test
// for the periapsis engix99 incident of 2026-07-31: once switched over to
// the kube-apiserver service address, the manager could never rotate back
// to a directly configured API server URL, on the assumption that Cilium's
// own datapath load balancing would route around a dead kube-apiserver.
// That assumption doesn't hold if the service has no healthy backends at
// all - there's nothing to load-balance to - and the agent was stuck
// retrying the unreachable service address indefinitely. This verifies
// that a heartbeat failure while connected to the service address now
// re-arms manual rotation, so the agent falls back to the originally
// configured API server URLs instead of latching permanently.
func Test_clientMultipleAPIServersServiceHeartbeatFallback(t *testing.T) {
	var requests lock.Map[string, *http.Request]
	getRequest := func(k string) *http.Request {
		v, _ := requests.Load(k)
		return v
	}
	apiStateFile, err := os.CreateTemp("", "kubeapi_state")
	require.NoError(t, err)
	defer apiStateFile.Close()
	K8sAPIServerFilePath = apiStateFile.Name()

	servers := make([]*httptest.Server, 3)
	for i := range servers {
		servers[i] = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Store(r.URL.Path, r)

			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/version":
				w.Write([]byte(`{
			       "major": "1",
			       "minor": "99"
			}`))
			default:
				w.Write([]byte("{}"))
			}
		}))
	}
	// servers[0] and servers[1] are the originally configured API server
	// URLs; both stay up for the whole test, representing the
	// last-known-good fallback the agent should return to.
	servers[0].Start()
	defer servers[0].Close()
	servers[1].Start()
	defer servers[1].Close()

	var (
		clientset Clientset
		mgr       *restConfigManager
	)
	h := hive.New(
		Cell,
		cell.Provide(
			loadbalancer.NewFrontendsTable, statedb.RWTable[*loadbalancer.Frontend].ToTable,
			func() loadbalancer.Config { return loadbalancer.DefaultConfig },
		),
		cell.Invoke(func(c Clientset, m *restConfigManager) { clientset = c; mgr = m }),
	)

	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	h.RegisterFlags(flags)
	urls := []string{servers[0].URL, servers[1].URL}
	flags.Set(option.K8sAPIServerURLs, strings.Join(urls, ","))
	flags.Set(option.K8sHeartbeatTimeout, "1s")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	tlog := hivetest.Logger(t)
	require.NoError(t, h.Start(tlog, ctx))
	defer func() { require.NoError(t, h.Stop(tlog, ctx)) }()

	// Check that we see the connection probe and version check.
	require.NotNil(t, getRequest("/api/v1/namespaces/kube-system"))
	require.NotNil(t, getRequest("/version"))

	// Switch over to the service address, as would happen once the agent
	// observes the default/kubernetes service frontend - no Endpoints
	// given, mirroring the real case where the backing EndpointSlice has
	// gone empty (see updateMappings: apiServerURLs is left untouched
	// when there are no endpoints to replace it with).
	servers[2].Start()
	mapping := K8sServiceEndpointMapping{Service: servers[2].URL}
	mgr.updateMappings(mapping)
	require.False(t, mgr.canRotateAPIServerURL(),
		"manual rotation must be gated off immediately after switching to the service address")

	require.NoError(t, testutils.WaitUntil(func() bool {
		_, err = clientset.CoreV1().Pods("test").Get(context.TODO(), "pod-via-service", metav1.GetOptions{})
		return err == nil
	}, 5*time.Second))
	require.NotNil(t, getRequest("/api/v1/namespaces/test/pods/pod-via-service"),
		"request must have gone through the service address")

	// The service address now has nothing to route to (e.g. the
	// EndpointSlice went empty) - simulate that by taking it down
	// entirely, and reset the heartbeat's success window so it doesn't
	// skip the next check.
	servers[2].Close()
	k8smetrics.LastSuccessInteraction.Reset()

	// The heartbeat should detect the failure, re-arm rotation, and fall
	// back to one of the originally configured (still-healthy) API
	// server URLs - not stay latched onto the now-dead service address.
	require.NoError(t, testutils.WaitUntil(func() bool {
		_, err = clientset.CoreV1().Pods("test").Get(context.TODO(), "pod-after-fallback", metav1.GetOptions{})
		return err == nil
	}, 5*time.Second))
	require.NotNil(t, getRequest("/api/v1/namespaces/test/pods/pod-after-fallback"),
		"request must have gone through after falling back from the dead service address")
	require.True(t, mgr.canRotateAPIServerURL(),
		"manual rotation must be re-armed after falling back from the service address")
}

func BenchmarkIsConnReady(b *testing.B) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			w.Write([]byte(`{
			       "major": "1",
			       "minor": "99"
			}`))
		default:
			w.Write([]byte("{}"))
		}
	}))
	server.Start()
	defer server.Close()

	var clientset Clientset
	h := hive.New(
		Cell,
		cell.Invoke(func(c Clientset) { clientset = c }),
	)

	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	h.RegisterFlags(flags)
	flags.Set(option.K8sAPIServerURLs, server.URL)
	// Bump up the settings for concurrent requests.
	flags.Set(option.K8sClientBurst, "100")
	flags.Set(option.K8sClientQPSLimit, "100")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	tlog := hivetest.Logger(b)
	require.NoError(b, h.Start(tlog, ctx))

	for b.Loop() {
		require.NoError(b, isConnReady(clientset))
	}

	require.NoError(b, h.Stop(tlog, ctx))
}

func BenchmarkIsConnReadyMultipleAPIServers(b *testing.B) {
	apiStateFile, err := os.CreateTemp("", "kubeapi_state")
	require.NoError(b, err)
	defer apiStateFile.Close()
	K8sAPIServerFilePath = apiStateFile.Name()

	servers := make([]*httptest.Server, 3)
	for i := range servers {
		servers[i] = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/version":
				w.Write([]byte(`{
			       "major": "1",
			       "minor": "99"
			}`))
			default:
				w.Write([]byte("{}"))
			}
		}))
	}
	servers[0].Start()
	servers[1].Start()
	servers[2].Start()

	var clientset Clientset
	h := hive.New(
		Cell,
		cell.Invoke(func(c Clientset) { clientset = c }),
	)

	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	h.RegisterFlags(flags)
	urls := []string{servers[0].URL, servers[1].URL, servers[2].URL}
	flags.Set(option.K8sAPIServerURLs, strings.Join(urls, ","))
	// Bump up the settings for concurrent requests.
	flags.Set(option.K8sClientBurst, "100")
	flags.Set(option.K8sClientQPSLimit, "100")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	tlog := hivetest.Logger(b)
	require.NoError(b, h.Start(tlog, ctx))

	num := 20
	for b.Loop() {
		var wg sync.WaitGroup
		wg.Add(num)
		for range num {
			go func() {
				require.NoError(b, isConnReady(clientset))
				wg.Done()
			}()
		}
		wg.Wait()
	}

	require.NoError(b, h.Stop(tlog, ctx))
}

// Test_clientAPIServerURLsRestoredOnDisconnect is a regression test for the
// SECOND half of the engifire ClusterIP deadlock (2026-08-02). The first fix
// cleared the isConnectedToService latch on heartbeat failure, but that alone
// was not enough and the agent stayed wedged in production:
//
//	"Lost connectivity to kubeapi service address"  x1       <- un-latch fired
//	"Rotated api server"                            x0       <- rotation did not
//	"no route to host"                              158595   <- dead ClusterIP
//
// Cause: updateMappings() REPLACES apiServerURLs with the service's endpoints
// on switchover, discarding the configured list. A single-apiserver cluster
// leaves one entry, so canRotateAPIServerURL()'s `len(...) > 1` is false
// forever and no rotation can occur regardless of the latch.
//
// This asserts the configured list is restored, so rotation is possible again.
func Test_clientAPIServerURLsRestoredOnDisconnect(t *testing.T) {
	mgr := &restConfigManager{
		log:        hivetest.Logger(t),
		rt:         &rotatingHttpRoundTripper{log: hivetest.Logger(t)},
		restConfig: &rest.Config{},
	}
	mgr.parseConfig(Config{SharedConfig: SharedConfig{K8sAPIServerURLs: []string{
		"https://10.0.0.1:6443",
		"https://10.0.0.2:6443",
		"https://10.0.0.3:6443",
	}}})
	require.Len(t, mgr.apiServerURLs, 3)
	require.Len(t, mgr.configuredAPIServerURLs, 3, "configured list must be captured at parse time")
	require.True(t, mgr.canRotateAPIServerURL())

	// Simulate switchover: the service had a SINGLE endpoint, which is what
	// collapses the list on a one-apiserver cluster.
	mgr.Lock()
	mgr.isConnectedToService = true
	mgr.apiServerURLs = []*url.URL{{Scheme: "https", Host: "10.96.0.1:443"}}
	mgr.restConfig.Host = "https://10.96.0.1:443"
	mgr.Unlock()
	require.False(t, mgr.canRotateAPIServerURL(),
		"precondition: after switchover with one endpoint, rotation is gated off")

	// The heartbeat failure path.
	mgr.disconnectFromService()

	require.False(t, mgr.isConnectedToService, "latch must clear")
	require.Len(t, mgr.apiServerURLs, 3,
		"the CONFIGURED urls must be restored - clearing the latch alone leaves a "+
			"single-entry list and rotation stays impossible, which is the live bug")
	require.True(t, mgr.canRotateAPIServerURL(),
		"rotation must be possible again after disconnect")

	// And it must actually be able to move off the dead service address.
	mgr.rotateAPIServerURL()
	require.NotEqual(t, "https://10.96.0.1:443", mgr.getConfig().Host,
		"must rotate away from the unreachable service address")
}

// Test_clientSingleConfiguredURLRestoredOnDisconnect covers the case
// rotateAPIServerURL() cannot handle: with exactly one configured apiserver it
// returns early, so nothing would move the host off the dead service address
// unless disconnectFromService() points it back explicitly.
func Test_clientSingleConfiguredURLRestoredOnDisconnect(t *testing.T) {
	mgr := &restConfigManager{
		log:        hivetest.Logger(t),
		rt:         &rotatingHttpRoundTripper{log: hivetest.Logger(t)},
		restConfig: &rest.Config{},
	}
	mgr.parseConfig(Config{SharedConfig: SharedConfig{K8sAPIServerURLs: []string{"https://10.0.0.1:6443"}}})

	mgr.Lock()
	mgr.isConnectedToService = true
	mgr.apiServerURLs = []*url.URL{{Scheme: "https", Host: "10.96.0.1:443"}}
	mgr.restConfig.Host = "https://10.96.0.1:443"
	mgr.Unlock()

	mgr.disconnectFromService()

	require.Equal(t, "https://10.0.0.1:6443", mgr.getConfig().Host,
		"with a single configured apiserver, rotateAPIServerURL() no-ops, so disconnect "+
			"must restore the host itself or the agent keeps dialling the dead ClusterIP")
}

// Test_clientAPIServerURLOrderIsPreference verifies --k8s-api-server-urls is an
// ORDERED PREFERENCE list: the first entry is used at startup and failures walk
// the rest in configured order. Upstream picked at random, so list order was
// meaningless and an operator could not express "prefer the wired path over the
// wireless one" - which is what motivated the change.
func Test_clientAPIServerURLOrderIsPreference(t *testing.T) {
	mgr := &restConfigManager{
		log:        hivetest.Logger(t),
		rt:         &rotatingHttpRoundTripper{log: hivetest.Logger(t)},
		restConfig: &rest.Config{},
	}
	mgr.parseConfig(Config{SharedConfig: SharedConfig{K8sAPIServerURLs: []string{
		"https://127.0.0.1:6443",       // preferred
		"https://192.168.50.1:6443",    // wired fallback
		"https://192.168.100.200:6443", // wireless, last resort
	}}})

	// Startup must land on the FIRST entry, deterministically. Under the old
	// random pick this was the preferred entry only ~1/3 of the time.
	mgr.selectPreferredAPIServerURL()
	require.Equal(t, "https://127.0.0.1:6443", mgr.getConfig().Host,
		"startup must use the operator's first choice")

	// Failures walk the list in order, then wrap.
	want := []string{
		"https://192.168.50.1:6443",
		"https://192.168.100.200:6443",
		"https://127.0.0.1:6443",
		"https://192.168.50.1:6443",
	}
	for i, w := range want {
		mgr.rotateAPIServerURL()
		require.Equal(t, w, mgr.getConfig().Host, "rotation %d must follow configured order", i+1)
	}
}

// Test_clientRotateResumesAtPreferredWhenCurrentUnknown covers the state after
// disconnectFromService: the host is the service address, which is not in the
// restored configured list. Rotation must resume at the most-preferred entry
// rather than getting stuck or picking arbitrarily.
func Test_clientRotateResumesAtPreferredWhenCurrentUnknown(t *testing.T) {
	mgr := &restConfigManager{
		log:        hivetest.Logger(t),
		rt:         &rotatingHttpRoundTripper{log: hivetest.Logger(t)},
		restConfig: &rest.Config{},
	}
	mgr.parseConfig(Config{SharedConfig: SharedConfig{K8sAPIServerURLs: []string{
		"https://127.0.0.1:6443",
		"https://192.168.50.1:6443",
	}}})

	// Current URL is the service address - not a member of the configured list.
	mgr.rt.apiServerURL = &url.URL{Scheme: "https", Host: "10.96.0.1:443"}

	mgr.rotateAPIServerURL()
	require.Equal(t, "https://127.0.0.1:6443", mgr.getConfig().Host,
		"an unknown current URL must resume at the preferred entry, not stall")
}

// Test_clientConfiguredURLsAreDeepCopied is a regression test for an aliasing
// bug: rotateAPIServerURL puts a *url.URL from apiServerURLs into
// rt.apiServerURL, and updateMappings then mutates that object IN PLACE
// (rt.apiServerURL.Host = mapping.Service). With a shallow copy the "configured"
// entry is rewritten to the service address, so restoring it on disconnect hands
// back the very address the fallback exists to escape.
func Test_clientConfiguredURLsAreDeepCopied(t *testing.T) {
	mgr := &restConfigManager{
		log:        hivetest.Logger(t),
		rt:         &rotatingHttpRoundTripper{log: hivetest.Logger(t)},
		restConfig: &rest.Config{},
	}
	mgr.parseConfig(Config{SharedConfig: SharedConfig{K8sAPIServerURLs: []string{
		"https://127.0.0.1:6443",
		"https://192.168.50.1:6443",
	}}})
	mgr.selectPreferredAPIServerURL()

	// Exactly what updateMappings does on service switchover.
	mgr.rt.apiServerURL.Host = "10.96.0.1:443"

	for i, u := range mgr.configuredAPIServerURLs {
		require.NotEqual(t, "10.96.0.1:443", u.Host,
			"configured entry %d was mutated through a shared pointer - restoring it "+
				"would hand back the dead service address", i)
	}
	require.Equal(t, "127.0.0.1:6443", mgr.configuredAPIServerURLs[0].Host)
}

// Test_clientAPIServerURLTiers verifies the TIERED form: each --k8s-api-server-urls
// value is one tier tried in order, and a value holding several whitespace-separated
// URLs is a tier whose members are equally preferred (shuffled per-process, so
// agents starting together spread across them rather than all picking the same one).
func Test_clientAPIServerURLTiers(t *testing.T) {
	newMgr := func() *restConfigManager {
		m := &restConfigManager{
			log:        hivetest.Logger(t),
			rt:         &rotatingHttpRoundTripper{log: hivetest.Logger(t)},
			restConfig: &rest.Config{},
		}
		m.parseConfig(Config{SharedConfig: SharedConfig{K8sAPIServerURLs: []string{
			"https://127.0.0.1:6443",                      // tier 1
			"https://10.0.0.1:6443 https://10.0.0.2:6443", // tier 2, equal preference
			"https://192.168.100.200:6443",                // tier 3
		}}})
		return m
	}

	m := newMgr()
	require.Len(t, m.apiServerURLs, 4, "all tier members must be flattened into the candidate list")

	// Tier boundaries must be respected regardless of intra-tier shuffling.
	require.Equal(t, "127.0.0.1:6443", m.apiServerURLs[0].Host, "tier 1 comes first")
	mid := []string{m.apiServerURLs[1].Host, m.apiServerURLs[2].Host}
	require.ElementsMatch(t, []string{"10.0.0.1:6443", "10.0.0.2:6443"}, mid,
		"tier 2's members occupy positions 1-2 in some order")
	require.Equal(t, "192.168.100.200:6443", m.apiServerURLs[3].Host, "tier 3 comes last")

	// Startup uses the most-preferred tier, and rotation walks tier 2 before tier 3.
	m.selectPreferredAPIServerURL()
	require.Equal(t, "https://127.0.0.1:6443", m.getConfig().Host)
	m.rotateAPIServerURL()
	require.Contains(t, []string{"https://10.0.0.1:6443", "https://10.0.0.2:6443"}, m.getConfig().Host)
	m.rotateAPIServerURL()
	require.Contains(t, []string{"https://10.0.0.1:6443", "https://10.0.0.2:6443"}, m.getConfig().Host)
	m.rotateAPIServerURL()
	require.Equal(t, "https://192.168.100.200:6443", m.getConfig().Host,
		"tier 3 is only reached after both tier-2 members")

	// Intra-tier order must actually vary across processes, otherwise the
	// "spread the load" half of the design does nothing. Two orders over many
	// constructions is enough; this is a shuffle, not a permutation test.
	seen := map[string]bool{}
	for range 40 {
		seen[newMgr().apiServerURLs[1].Host] = true
	}
	require.Len(t, seen, 2, "tier members must be shuffled per-process, not fixed")
}

// The exported rotation hook is what lets work that runs during hive
// object-graph population (managed-node discovery) fall through the tier list
// before upstream's own connection loop has had a chance to run.
func Test_clientRotateAPIServerURLHook(t *testing.T) {
	newClient := func(urls ...string) *compositeClientset {
		m := &restConfigManager{
			log:        hivetest.Logger(t),
			rt:         &rotatingHttpRoundTripper{log: hivetest.Logger(t)},
			restConfig: &rest.Config{},
		}
		m.parseConfig(Config{SharedConfig: SharedConfig{K8sAPIServerURLs: urls}})
		m.selectPreferredAPIServerURL()
		return &compositeClientset{restConfigManager: m}
	}

	// Walks the tiers in preference order and wraps around.
	c := newClient("https://127.0.0.1:6443", "https://192.168.50.1:6443")
	require.Equal(t, "https://127.0.0.1:6443", c.restConfigManager.getConfig().Host)
	require.True(t, c.RotateAPIServerURL())
	require.Equal(t, "https://192.168.50.1:6443", c.restConfigManager.getConfig().Host)
	require.True(t, c.RotateAPIServerURL())
	require.Equal(t, "https://127.0.0.1:6443", c.restConfigManager.getConfig().Host)

	// Nothing to fall through to.
	single := newClient("https://127.0.0.1:6443")
	require.False(t, single.RotateAPIServerURL())
	require.Equal(t, "https://127.0.0.1:6443", single.restConfigManager.getConfig().Host)

	// Once graduated onto the service address the datapath load balances
	// across live backends; manual rotation would fight it.
	c.restConfigManager.isConnectedToService = true
	host := c.restConfigManager.getConfig().Host
	require.False(t, c.RotateAPIServerURL())
	require.Equal(t, host, c.restConfigManager.getConfig().Host)

	// A disabled clientset has no manager at all.
	require.False(t, (&compositeClientset{}).RotateAPIServerURL())
}

// The hook is reached through an optional interface, so it is only wired up if
// what the hive provides really is the type carrying the method. A wrapper
// introduced later would silently turn fall-through back into a plain retry.
func Test_clientProvidedClientsetSupportsRotation(t *testing.T) {
	var cs Clientset = &compositeClientset{}
	_, ok := cs.(interface{ RotateAPIServerURL() bool })
	require.True(t, ok, "the concrete type provided as Clientset must carry the rotation hook")
}

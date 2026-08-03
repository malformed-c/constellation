// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cilium/hive/cell"
	"github.com/cilium/hive/job"
	"github.com/cilium/statedb"
	"github.com/cloudflare/cfssl/log"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/cilium/cilium/pkg/loadbalancer"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/time"
	"github.com/cilium/cilium/pkg/version"
)

var (
	// K8sAPIServerFilePath is the file path for storing kube-apiserver service and
	// endpoints for high availability failover.
	K8sAPIServerFilePath = filepath.Join(option.Config.StateDir, "k8sapi_server_state.json")
)

type K8sServiceEndpointMapping struct {
	Service   string   `json:"service"`
	Endpoints []string `json:"endpoints"`
}

func (m K8sServiceEndpointMapping) Equal(other K8sServiceEndpointMapping) bool {
	return m.Service == other.Service && slices.Equal(m.Endpoints, other.Endpoints)
}

// restConfigManager manages the rest configuration for connecting to the API server, including the logic to fail over
// to an active kube-apiserver in order to support high availability.
//
// Below are the sequence of events to support kube-apiserver failover.
//
// Bootstrap: It parses the user provided configuration which may include multiple API server URLs. In case of multiple
// API servers, it wraps the rest configuration with an HTTP RoundTripper that enables updating the remote host while
// making API requests to the kube-apiserver. It also asynchronously monitors kube-apiserver service and endpoints related updates.
// The list is an ORDERED PREFERENCE list in this fork (upstream picks at random): the first entry is used at startup and
// later entries are fallbacks, tried in configured order on connectivity failures. See rotateAPIServerURL for why.
//
// Runtime: After the agent's initial sync with the kube-apiserver, when the manager receives updates for the kube-apiserver
// service, it switches over to the service address as the remote host set in the rest configuration. Thereafter, manual
// rotation of API servers is not needed as Cilium datapath will load-balance API traffic to the kube-apiserver endpoints.
//
// Restore: The manager restores the persisted kube-apiserver state after restart after ensuring connectivity using
// the service address. If that fails, it'll fall back to user provided kube-apiserver URLs. Note that these could be
// different from the ones configured during initial bootstrap as those kube-apiservers may all have been rotated while
// the agent was down.
type restConfigManager struct {
	restConfig    *rest.Config
	apiServerURLs []*url.URL
	// configuredAPIServerURLs is the operator-supplied --k8s-api-server-urls
	// list, kept verbatim. apiServerURLs is REPLACED by the kube-apiserver
	// service's endpoints on switchover (see updateMappings), which throws
	// the configured list away - so without this copy there is nothing to
	// fall back to when the service address later stops working.
	configuredAPIServerURLs []*url.URL
	isConnectedToService    bool
	lock.RWMutex
	log *slog.Logger
	rt  *rotatingHttpRoundTripper
}

func (r *restConfigManager) getConfig() *rest.Config {
	r.RLock()
	defer r.RUnlock()
	return rest.CopyConfig(r.restConfig)
}

func (r *restConfigManager) canRotateAPIServerURL() bool {
	r.RLock()
	defer r.RUnlock()

	// API server URLs are initially manually rotated when multiple
	// servers are configured by the user. Once the connections are
	// switched over to the kube-apiserver service address, manual
	// rotation isn't needed as Cilium datapath will load balance
	// connections to active kube-apiservers.
	return len(r.apiServerURLs) > 1 && !r.isConnectedToService
}

// disconnectFromService un-latches isConnectedToService, so a subsequent
// canRotateAPIServerURL call can return true again. Without this, once
// isConnectedToService is set the manager can never manually rotate away
// from the kube-apiserver service address again - fine as long as the
// datapath's own load balancing can route around a dead kube-apiserver,
// but that assumption breaks if the service has no healthy backends at
// all: there's nothing for the datapath to load-balance to, and the
// agent needs a way back to the last-known-good API server URLs (either
// the original configuration, or the endpoints from the last successful
// service update) instead of retrying an address it cannot route
// through. Intended to be called from a connectivity-failure path (e.g.
// the heartbeat's onFailure, see cell.go's rotateAPIServer) immediately
// before checking canRotateAPIServerURL, not on every failed request.
func (r *restConfigManager) disconnectFromService() {
	r.Lock()
	wasConnected := r.isConnectedToService
	r.isConnectedToService = false

	// Clearing the latch is NOT sufficient on its own, and this was the half
	// missing from the original fix. updateMappings() replaces apiServerURLs
	// with the service's ENDPOINTS on switchover, discarding the configured
	// list. On a single-apiserver cluster that leaves exactly one entry, so
	// canRotateAPIServerURL()'s `len(apiServerURLs) > 1` stays false and no
	// rotation can ever happen no matter what the latch says - observed live
	// on engifire, where the un-latch fired once and the agent then dialled a
	// dead ClusterIP 158595 times. Restore the configured list so there is
	// something to rotate to.
	restored := false
	if len(r.configuredAPIServerURLs) > 0 {
		// Deep copy again on the way out, so the pristine configured list
		// survives any later in-place mutation of the active one.
		r.apiServerURLs = cloneURLs(r.configuredAPIServerURLs)
		restored = true
	}
	r.Unlock()

	if !wasConnected {
		return
	}

	r.log.Warn("Lost connectivity to kubeapi service address, falling back to manual API server rotation",
		logfields.Count, len(r.configuredAPIServerURLs),
	)

	// rotateAPIServerURL() returns early when only one URL is configured, so
	// with a single configured apiserver nothing would move the host off the
	// dead service address. Point it back explicitly.
	if restored && len(r.configuredAPIServerURLs) == 1 {
		r.rt.Lock()
		r.rt.apiServerURL = r.configuredAPIServerURLs[0]
		r.rt.Unlock()

		r.Lock()
		r.restConfig.Host = r.configuredAPIServerURLs[0].String()
		r.Unlock()

		r.log.Info("Restored single configured api server",
			logfields.URL, r.configuredAPIServerURLs[0],
		)
	}
}

func restConfigManagerInit(cfg Config, name string, log *slog.Logger) (*restConfigManager, error) {
	var err error
	manager := restConfigManager{
		log: log,
		rt: &rotatingHttpRoundTripper{
			log: log,
		},
	}

	manager.parseConfig(cfg)

	cmdName := "cilium"
	if len(os.Args[0]) != 0 {
		cmdName = filepath.Base(os.Args[0])
	}
	userAgent := fmt.Sprintf("%s/%s", cmdName, version.Version)

	if name != "" {
		userAgent = fmt.Sprintf("%s %s", userAgent, name)
	}

	if manager.restConfig, err = manager.createConfig(cfg, userAgent); err != nil {
		return nil, err
	}
	if manager.canRotateAPIServerURL() {
		// Start on the operator's FIRST choice. Upstream rotated here (i.e.
		// picked at random); with an ordered preference list that would mean
		// never using the preferred entry at startup, which is the opposite
		// of what the list now means.
		manager.selectPreferredAPIServerURL()

		// Restore the mappings from disk.
		manager.restoreFromDisk()
	}

	return &manager, err
}

// createConfig creates a rest.Config for connecting to k8s api-server.
//
// The precedence of the configuration selection is the following:
// 1. kubeCfgPath
// 2. apiServerURL(s) (https if specified)
// 3. rest.InClusterConfig().
func (r *restConfigManager) createConfig(cfg Config, userAgent string) (*rest.Config, error) {
	var (
		config       *rest.Config
		err          error
		apiServerURL string
	)
	if len(r.apiServerURLs) > 0 {
		apiServerURL = r.apiServerURLs[0].String()
	}
	kubeCfgPath := cfg.K8sKubeConfigPath
	qps := cfg.K8sClientQPS
	burst := cfg.K8sClientBurst

	switch {
	// If the apiServerURL and the kubeCfgPath are empty then we can try getting
	// the rest.Config from the InClusterConfig
	case apiServerURL == "" && kubeCfgPath == "":
		if config, err = rest.InClusterConfig(); err != nil {
			return nil, err
		}
	case kubeCfgPath != "":
		if config, err = clientcmd.BuildConfigFromFlags("", kubeCfgPath); err != nil {
			return nil, err
		}
	case strings.HasPrefix(apiServerURL, "https://"):
		if config, err = rest.InClusterConfig(); err != nil {
			return nil, err
		}
		config.Host = apiServerURL
	default:
		config = &rest.Config{Host: apiServerURL, UserAgent: userAgent}
	}

	// The HTTP round tripper rotates API server URLs in case of connectivity failures.
	if len(r.apiServerURLs) > 1 {
		config.Wrap(r.WrapRoundTripper)
	}

	setConfig(config, userAgent, qps, burst)
	return config, nil
}

// parseConfig builds the API server candidate list from --k8s-api-server-urls.
//
// The list is TIERED. Each flag value is one tier, and a value may hold several
// whitespace-separated URLs which share that tier:
//
//	--k8s-api-server-urls=https://127.0.0.1:6443
//	--k8s-api-server-urls="https://10.0.0.1:6443 https://10.0.0.2:6443"
//	--k8s-api-server-urls=https://192.168.100.200:6443
//
// Tiers are tried in the order given (see rotateAPIServerURL); members WITHIN a
// tier are equally preferred, so they are shuffled per-process here. That gives
// both properties at once: a strict preference between tiers ("wired before
// wireless"), and load spreading across genuinely equivalent API servers, which
// is what upstream's blanket randomisation was for. Flattening the tiers into
// one ordered slice means every downstream user - createConfig's [0],
// canRotateAPIServerURL's len check, updateMappings' wholesale replacement -
// keeps working unchanged.
//
// Whitespace is the intra-tier separator because pflag's StringSlice already
// splits values on commas, so a comma cannot mean "same tier" here.
func (r *restConfigManager) parseConfig(cfg Config) {
	for _, tierSpec := range cfg.K8sAPIServerURLs {
		var tier []*url.URL

		for apiServerURL := range strings.FieldsSeq(tierSpec) {
			if !strings.HasPrefix(apiServerURL, "http") && !strings.HasPrefix(apiServerURL, "https") {
				apiServerURL = fmt.Sprintf("https://%s", apiServerURL)
			}

			serverURL, err := url.Parse(apiServerURL)
			if err != nil {
				r.log.Error("Failed to parse APIServerURL, skipping",
					logfields.Error, err,
					logfields.URL, apiServerURL,
				)
				continue
			}

			tier = append(tier, serverURL)
		}

		// Equal preference within a tier: shuffle so that N agents starting
		// together do not all land on the same member.
		if len(tier) > 1 {
			rand.Shuffle(len(tier), func(i, j int) { tier[i], tier[j] = tier[j], tier[i] })
		}
		r.apiServerURLs = append(r.apiServerURLs, tier...)
	}

	// Keep the configured list intact for disconnectFromService(). This must
	// be a DEEP copy, not append(nil, ...): rotateAPIServerURL aliases an
	// element of apiServerURLs into rt.apiServerURL, and updateMappings then
	// mutates that object in place (`rt.apiServerURL.Host = mapping.Service`).
	// A shallow copy shares those pointers, so switchover would rewrite the
	// "configured" entry to the service address and restoring it would hand
	// back the very address we are trying to escape.
	r.configuredAPIServerURLs = cloneURLs(r.apiServerURLs)
}

// cloneURLs returns a deep copy, so callers cannot mutate the originals
// through the returned slice (see parseConfig for why that matters).
func cloneURLs(in []*url.URL) []*url.URL {
	out := make([]*url.URL, 0, len(in))
	for _, u := range in {
		c := *u
		out = append(out, &c)
	}
	return out
}

func setConfig(config *rest.Config, userAgent string, qps float32, burst int) {
	if userAgent != "" {
		config.UserAgent = userAgent
	}
	if qps != 0.0 {
		config.QPS = qps
	}
	if burst != 0 {
		config.Burst = burst
	}
}

// rotateAPIServerURL advances to the NEXT --k8s-api-server-urls entry in the
// order the operator configured, wrapping at the end.
//
// Upstream picks at random here, which spreads load when the URLs are distinct
// API servers. In this fork they are typically several NETWORK PATHS TO THE
// SAME control plane (a KAS built with plural --bind-addresses: loopback, a
// wired LAN address, a wireless one), so there is no load to spread and random
// selection instead means each agent start lands on an arbitrary path. That
// cost is real and was measured: agents landing on the wireless address were
// implicated in leader-election churn elsewhere on this cluster, and list order
// could not express "prefer the wired path" because order was ignored.
//
// Ordered rotation makes the list a PREFERENCE list - first entry is used at
// startup, later ones are fallbacks tried in turn on failure - which is what a
// list normally means and what the values.yaml comment can now honestly say.
// The trade-off, stated so it is not rediscovered: on a genuine multi-apiserver
// deployment every agent now prefers the same first entry instead of spreading.
func (r *restConfigManager) rotateAPIServerURL() {
	if len(r.apiServerURLs) <= 1 {
		return
	}

	r.rt.Lock()
	defer r.rt.Unlock()

	// Find where we are and step one on. Compare by value, not pointer: the
	// active list is replaced wholesale on switchover and restore, so pointer
	// identity does not survive. An unknown current URL (nil, or the service
	// address after a disconnect) falls through to index 0 - the most
	// preferred entry - which is the right place to resume from.
	next := 0
	if cur := r.rt.apiServerURL; cur != nil {
		for i, u := range r.apiServerURLs {
			if u.String() == cur.String() {
				next = (i + 1) % len(r.apiServerURLs)
				break
			}
		}
	}
	r.rt.apiServerURL = r.apiServerURLs[next]

	r.Lock()
	r.restConfig.Host = r.rt.apiServerURL.String()
	r.Unlock()
	r.log.Info("Rotated api server",
		logfields.URL, r.rt.apiServerURL,
		logfields.Index, next,
	)
}

// selectPreferredAPIServerURL points the client at the FIRST configured URL.
// Used at startup: rt.apiServerURL begins nil and RoundTrip dereferences it, so
// something must set it, and with an ordered list that something must be the
// most-preferred entry rather than a rotation off it.
func (r *restConfigManager) selectPreferredAPIServerURL() {
	if len(r.apiServerURLs) == 0 {
		return
	}

	r.rt.Lock()
	r.rt.apiServerURL = r.apiServerURLs[0]
	r.rt.Unlock()

	r.Lock()
	r.restConfig.Host = r.apiServerURLs[0].String()
	r.Unlock()

	r.log.Info("Selected preferred api server",
		logfields.URL, r.apiServerURLs[0],
	)
}

// rotatingHttpRoundTripper sets the remote host in the rest configuration used to make API requests to the API server.
type rotatingHttpRoundTripper struct {
	delegate     http.RoundTripper
	log          *slog.Logger
	apiServerURL *url.URL
	lock.RWMutex // Synchronizes access to apiServerURL
}

func (rt *rotatingHttpRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.RLock()
	defer rt.RUnlock()

	rt.log.Debug("Kubernetes api server host",
		logfields.URL, rt.apiServerURL,
	)
	req.URL.Host = rt.apiServerURL.Host
	return rt.delegate.RoundTrip(req)
}

func (r *restConfigManager) WrapRoundTripper(rt http.RoundTripper) http.RoundTripper {
	r.rt.delegate = rt
	return r.rt
}

func (r *restConfigManager) restoreFromDisk() {
	f, err := os.Open(K8sAPIServerFilePath)
	if err != nil {
		r.log.Error("unable "+
			"to open file, agent may not be able to fail over to an active kube-apiserver",
			logfields.Path, K8sAPIServerFilePath,
			logfields.Error, err,
		)
		return
	}
	defer f.Close()

	if finfo, err := os.Stat(K8sAPIServerFilePath); err != nil || finfo.Size() == 0 {
		return
	}

	var mapping K8sServiceEndpointMapping
	if err = json.NewDecoder(f).Decode(&mapping); err != nil {
		r.log.Error("failed to "+
			"decode file entry, agent may not be able to fail over to an active kube-apiserver",
			logfields.Error, err,
			logfields.Path, K8sAPIServerFilePath,
			logfields.Entry, mapping,
		)
		return
	}
	r.updateMappings(mapping)
}

func (r *restConfigManager) saveMapping(mapping K8sServiceEndpointMapping) {
	// Write the mappings to disk so they can be restored on restart.
	if f, err := os.OpenFile(K8sAPIServerFilePath, os.O_RDWR, 0644); err == nil {
		defer f.Close()
		if err = json.NewEncoder(f).Encode(mapping); err != nil {
			log.Error("failed to write kubernetes service entry,"+
				"agent may not be able to fail over to an active k8sapi-server",
				logfields.Error, err,
				logfields.Entry, mapping,
			)
		}
	}
}

func (r *restConfigManager) updateMappings(mapping K8sServiceEndpointMapping) {
	if err := r.checkConnToService(mapping.Service); err != nil {
		return
	}

	r.saveMapping(mapping)

	r.log.Info("Updated kubeapi server url host",
		logfields.URL, mapping.Service,
	)

	// Set in tests
	mapping.Service = strings.TrimPrefix(mapping.Service, "http://")
	r.rt.Lock()
	defer r.rt.Unlock()
	r.rt.apiServerURL.Host = mapping.Service
	r.Lock()
	defer r.Unlock()
	r.isConnectedToService = true
	r.restConfig.Host = mapping.Service
	updatedServerURLs := make([]*url.URL, 0)
	for _, endpoint := range mapping.Endpoints {
		endpoint = fmt.Sprintf("https://%s", endpoint)
		serverURL, err := url.Parse(endpoint)
		if err != nil {
			r.log.Info("Failed to parse endpoint, skipping",
				logfields.Endpoint, endpoint,
				logfields.Error, err,
			)
			continue
		}
		updatedServerURLs = append(updatedServerURLs, serverURL)
	}
	if len(updatedServerURLs) != 0 {
		r.apiServerURLs = updatedServerURLs
	}

}

// checkConnToService ensures connectivity to the API server via the passed service address.
func (r *restConfigManager) checkConnToService(host string) error {
	stop := make(chan struct{})
	timeout := time.NewTimer(connTimeout)
	defer timeout.Stop()
	var (
		config *rest.Config
		err    error
	)
	if strings.HasPrefix(host, "http") {
		config = &rest.Config{Host: host, Timeout: connTimeout}
	} else {
		hostURL := fmt.Sprintf("https://%s", host)
		config, err = rest.InClusterConfig()
		if err != nil {
			r.log.Error("unable to read cluster config",
				logfields.Error, err,
			)
			return err
		}
		config.Host = hostURL
	}
	wait.Until(func() {
		r.log.Info("Checking connection to kubeapi service",
			logfields.Address, config.Host,
		)
		httpClient, _ := rest.HTTPClientFor(config)

		cs, _ := kubernetes.NewForConfigAndClient(config, httpClient)
		if err = isConnReady(cs); err == nil {
			close(stop)
			return
		}

		select {
		case <-timeout.C:
		default:
			return
		}

		r.log.Error("kubeapi service not ready yet",
			logfields.Address, config.Host,
			logfields.Error, err,
		)
		close(stop)
	}, connRetryInterval, stop)
	if err == nil {
		r.log.Info("Connected to kubeapi service",
			logfields.Address, config.Host,
		)
	}
	return err
}

type mappingUpdaterParams struct {
	cell.In

	JobGroup  job.Group
	Log       *slog.Logger
	Manager   *restConfigManager
	DB        *statedb.DB                           `optional:"true"`
	Frontends statedb.Table[*loadbalancer.Frontend] `optional:"true"`
}

// registerMappingsUpdater watches the default/kubernetes frontend for
// changes and updates the mapping file.
// This is currently used for supporting high availability for kubeapi-server.
func registerMappingsUpdater(p mappingUpdaterParams) {
	if p.DB == nil || p.Frontends == nil {
		// These are optional to make the [Cell] usable without
		// load-balancing control-plane.
		return
	}

	if p.Manager == nil || !p.Manager.canRotateAPIServerURL() {
		return
	}

	p.JobGroup.Add(
		job.OneShot(
			"update-k8s-api-service-mappings",
			func(ctx context.Context, health cell.Health) error {
				// Watch for changes to the default/kubernetes service frontend
				// and update the mappings if it changes.
				var previous K8sServiceEndpointMapping
				for {
					fe, _, watch, found := p.Frontends.GetWatch(
						p.DB.ReadTxn(),
						loadbalancer.FrontendByServiceName(loadbalancer.NewServiceName(
							"default", "kubernetes")))
					if found {
						mapping := frontendToMapping(fe)
						if !mapping.Equal(previous) {
							previous = mapping
							log.Info("updating kubernetes service mapping",
								logfields.Entry, mapping,
							)
							p.Manager.updateMappings(mapping)

						}
					}

					select {
					case <-ctx.Done():
						return nil
					case <-watch:
					}
				}
			}))
}

func frontendToMapping(fe *loadbalancer.Frontend) K8sServiceEndpointMapping {
	var mapping K8sServiceEndpointMapping
	mapping.Service = fe.Address.AddrString()
	for be := range fe.Backends {
		mapping.Endpoints = append(mapping.Endpoints, be.Address.AddrString())
	}
	return mapping
}

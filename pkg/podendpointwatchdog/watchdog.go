// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

// Package podendpointwatchdog periodically detects pods that are Running
// with a PodIP assigned but have no corresponding local Cilium endpoint —
// e.g. because the endpoint was lost across an agent restart while the
// pod's netns/IP lingered and nothing re-triggered CNI ADD — and deletes
// them so Kubernetes recreates them with a fresh CNI ADD.
package podendpointwatchdog

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"

	"github.com/cilium/hive/cell"
	"github.com/cilium/hive/job"
	"github.com/cilium/statedb"
	"github.com/spf13/pflag"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/cilium/cilium/pkg/endpoint"
	"github.com/cilium/cilium/pkg/endpointmanager"
	"github.com/cilium/cilium/pkg/endpointstate"
	k8sClient "github.com/cilium/cilium/pkg/k8s/client"
	slim_corev1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/core/v1"
	"github.com/cilium/cilium/pkg/k8s/tables"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/promise"
	"github.com/cilium/cilium/pkg/time"
)

const jobName = "pod-endpoint-watchdog"

type Config struct {
	EnablePodEndpointWatchdog      bool
	PodEndpointWatchdogInterval    time.Duration
	PodEndpointWatchdogGracePeriod time.Duration
}

func (c Config) Flags(flags *pflag.FlagSet) {
	flags.Bool("enable-pod-endpoint-watchdog", c.EnablePodEndpointWatchdog,
		"Periodically detect Running pods with no local Cilium endpoint (e.g. lost across an agent restart) and delete them to trigger a fresh CNI ADD")
	flags.Duration("pod-endpoint-watchdog-interval", c.PodEndpointWatchdogInterval,
		"Interval between pod-endpoint watchdog scans")
	flags.Duration("pod-endpoint-watchdog-grace-period", c.PodEndpointWatchdogGracePeriod,
		"Minimum duration a pod must continuously observe as missing its Cilium endpoint before pod-endpoint watchdog heals it")
}

// Cell registers the pod-endpoint watchdog.
var Cell = cell.Module(
	"pod-endpoint-watchdog",
	"Detects and heals Running pods with no local Cilium endpoint",

	cell.Config(Config{
		EnablePodEndpointWatchdog:      true,
		PodEndpointWatchdogInterval:    60 * time.Second,
		PodEndpointWatchdogGracePeriod: 90 * time.Second,
	}),
	cell.Invoke(registerWatchdog),
)

// endpointLookup is the narrow slice of endpointmanager.EndpointManager this
// package needs, kept minimal so tests can fake it without a real manager.
type endpointLookup interface {
	LookupIP(ip netip.Addr) *endpoint.Endpoint
}

// podDeleter is the narrow slice of k8sClient.Clientset this package needs.
type podDeleter interface {
	DeletePod(ctx context.Context, namespace, name string, uid k8stypes.UID) error
}

type clientsetPodDeleter struct {
	clientset k8sClient.Clientset
}

func (d clientsetPodDeleter) DeletePod(ctx context.Context, namespace, name string, uid k8stypes.UID) error {
	return d.clientset.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
	})
}

type params struct {
	cell.In

	Config   Config
	Logger   *slog.Logger
	JobGroup job.Group
	DB       *statedb.DB
	PodTable statedb.Table[tables.LocalPod]

	EndpointManager endpointmanager.EndpointManager
	Clientset       k8sClient.Clientset

	RestorerPromise promise.Promise[endpointstate.Restorer]
}

func registerWatchdog(p params) {
	if !p.Config.EnablePodEndpointWatchdog || !p.Clientset.IsEnabled() {
		return
	}

	w := &watchdog{
		logger:      p.Logger,
		db:          p.DB,
		podTable:    p.PodTable,
		endpoints:   p.EndpointManager,
		podDeleter:  clientsetPodDeleter{clientset: p.Clientset},
		gracePeriod: p.Config.PodEndpointWatchdogGracePeriod,
		now:         time.Now,
		pending:     make(map[k8stypes.UID]time.Time),
	}

	p.JobGroup.Add(job.OneShot("wait-for-endpoint-restore", func(ctx context.Context, _ cell.Health) error {
		restorer, err := p.RestorerPromise.Await(ctx)
		if err != nil {
			return fmt.Errorf("failed to wait for endpoint restorer promise: %w", err)
		}
		if err := restorer.WaitForEndpointRestore(ctx); err != nil {
			return fmt.Errorf("failed to wait for endpoint restoration: %w", err)
		}

		p.JobGroup.Add(job.Timer(jobName, w.check, p.Config.PodEndpointWatchdogInterval))
		return nil
	}))
}

type watchdog struct {
	logger *slog.Logger

	db       *statedb.DB
	podTable statedb.Table[tables.LocalPod]

	endpoints  endpointLookup
	podDeleter podDeleter

	gracePeriod time.Duration

	// now is overridable in tests for a deterministic clock; production
	// wiring always sets it to time.Now.
	now func() time.Time

	// pending tracks pods observed as Running-with-IP-but-no-endpoint, keyed
	// by UID, with the time each was FIRST observed that way. A pod is only
	// healed once that has been continuously true for at least gracePeriod,
	// so a single transient/racy read (e.g. a pod whose CNI ADD is still in
	// flight) never triggers a delete on its own.
	//
	// This is deliberately self-contained rather than trusting any
	// pod-provided timestamp (e.g. status.startTime): not every control
	// plane populates one reliably, and one that gets bumped by container
	// restarts would silently defeat the grace period entirely.
	pending map[k8stypes.UID]time.Time
}

func (w *watchdog) check(ctx context.Context) error {
	txn := w.db.ReadTxn()
	now := w.now()
	seen := make(map[k8stypes.UID]struct{})

	for pod := range w.podTable.All(txn) {
		if pod.Spec.HostNetwork {
			// hostNetwork pods share the host's network namespace and are
			// never assigned their own Cilium endpoint.
			continue
		}
		if pod.DeletionTimestamp != nil {
			// Already terminating; leave it to run its course.
			continue
		}
		if pod.Status.Phase != slim_corev1.PodRunning || pod.Status.PodIP == "" {
			continue
		}

		addr, err := netip.ParseAddr(pod.Status.PodIP)
		if err != nil {
			continue
		}
		if w.endpoints.LookupIP(addr) != nil {
			continue // healthy
		}

		seen[pod.UID] = struct{}{}
		firstSeen, alreadyPending := w.pending[pod.UID]
		if !alreadyPending {
			w.pending[pod.UID] = now
			w.logger.Debug("Pod missing its local Cilium endpoint; starting grace period",
				logfields.K8sPodName, pod.Name,
				logfields.K8sNamespace, pod.Namespace,
				logfields.IPAddr, pod.Status.PodIP,
			)
			continue
		}
		if now.Sub(firstSeen) >= w.gracePeriod {
			w.heal(ctx, pod)
		}
	}

	// Forget any pod that's no longer missing its endpoint (healed itself,
	// was deleted, or simply left the table), so the grace period re-applies
	// from scratch if it ever reappears.
	for uid := range w.pending {
		if _, ok := seen[uid]; !ok {
			delete(w.pending, uid)
		}
	}

	return nil
}

func (w *watchdog) heal(ctx context.Context, pod tables.LocalPod) {
	scopedLog := w.logger.With(
		logfields.K8sPodName, pod.Name,
		logfields.K8sNamespace, pod.Namespace,
		logfields.IPAddr, pod.Status.PodIP,
	)
	scopedLog.Warn("Pod is Running with an IP but has no local Cilium endpoint; deleting to force a fresh CNI ADD")

	err := w.podDeleter.DeletePod(ctx, pod.Namespace, pod.Name, pod.UID)
	if err != nil && !k8serrors.IsNotFound(err) {
		scopedLog.Error("Failed to delete pod with missing Cilium endpoint", logfields.Error, err)
	}
}

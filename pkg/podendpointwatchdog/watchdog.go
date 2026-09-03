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
	"github.com/cilium/cilium/pkg/node"
	"github.com/cilium/cilium/pkg/promise"
	"github.com/cilium/cilium/pkg/time"
)

const jobName = "pod-endpoint-watchdog"

// blastRadiusFloor and blastRadiusFraction bound how many pods one scan may
// delete: max(floor, fraction of the node's pod-network pods).
//
// The floor cannot be below a small number: a node legitimately running one or
// two pods that lost their endpoints has a high missing-fraction, and that is
// the case the watchdog exists to fix. The fraction is what catches the shape
// that actually hurt -- most or all of a node at once -- on nodes large enough
// that a flat floor would be no protection at all.
const (
	blastRadiusFloor    = 3
	blastRadiusFraction = 0.2
)

// blastRadiusLimit is the most pods a single scan may delete on a node with
// the given number of pod-network pods.
func blastRadiusLimit(eligible int) int {
	byFraction := int(float64(eligible) * blastRadiusFraction)
	return max(blastRadiusFloor, byFraction)
}

type Config struct {
	EnablePodEndpointWatchdog      bool
	PodEndpointWatchdogInterval    time.Duration
	PodEndpointWatchdogGracePeriod time.Duration
	PodEndpointWatchdogCNIConfDir  string
}

func (c Config) Flags(flags *pflag.FlagSet) {
	flags.Bool("enable-pod-endpoint-watchdog", c.EnablePodEndpointWatchdog,
		"Periodically detect Running pods with no local Cilium endpoint (e.g. lost across an agent restart) and delete them to trigger a fresh CNI ADD")
	flags.Duration("pod-endpoint-watchdog-interval", c.PodEndpointWatchdogInterval,
		"Interval between pod-endpoint watchdog scans")
	flags.Duration("pod-endpoint-watchdog-grace-period", c.PodEndpointWatchdogGracePeriod,
		"Minimum duration a pod must continuously observe as missing its Cilium endpoint before pod-endpoint watchdog heals it")
	flags.String("pod-endpoint-watchdog-cni-conf-dir", c.PodEndpointWatchdogCNIConfDir,
		"Directory holding this node's CNI configuration. The pod-endpoint watchdog only acts when a configuration here names cilium-cni, so it never deletes pods that arrived through another CNI")
}

// Cell registers the pod-endpoint watchdog.
var Cell = cell.Module(
	"pod-endpoint-watchdog",
	"Detects and heals Running pods with no local Cilium endpoint",

	cell.Config(Config{
		EnablePodEndpointWatchdog:      true,
		PodEndpointWatchdogInterval:    60 * time.Second,
		PodEndpointWatchdogGracePeriod: 90 * time.Second,
		PodEndpointWatchdogCNIConfDir:  DefaultCNIConfDir,
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
	LocalNodeStore  *node.LocalNodeStore
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
		cniOwned: func(ctx context.Context) (bool, string) {
			// Primary authority: what perigeos says it is ACTUALLY running.
			ln, err := p.LocalNodeStore.Get(ctx)
			if err != nil {
				return false, fmt.Sprintf("cannot read local node: %v", err)
			}
			owned, reason := nodeLabelSaysCilium(ln.Labels)
			if !owned {
				return false, reason
			}
			// Secondary, never sole: the conflist must also be there. Guards a
			// label left behind after the backend moved on. Named separately in
			// the log so a silent disable from a path change is diagnosable
			// rather than looking like an honest "not ours".
			if ok, why := nodeUsesCiliumCNI(p.Config.PodEndpointWatchdogCNIConfDir); !ok {
				return false, fmt.Sprintf("%s but %s", reason, why)
			}
			return true, reason
		},
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

	// cniOwned reports whether this node's pods arrive through cilium-cni.
	// The watchdog does nothing at all unless they do; see nodeUsesCiliumCNI.
	cniOwned cniOwnership

	// standDownLogged keeps the "not our CNI" message to once per process
	// rather than once per scan interval, since it is a steady state on a
	// node we do not manage, not an event.
	standDownLogged bool
}

func (w *watchdog) check(ctx context.Context) error {
	// Establish that this node's pods are ours before treating a missing
	// endpoint as a fault. On a node running another CNI every pod-network
	// pod looks broken, and healing them deletes another CNI's workloads in
	// a loop.
	if owned, reason := w.cniOwned(ctx); !owned {
		if !w.standDownLogged {
			w.standDownLogged = true
			w.logger.Info(
				"Pod-endpoint watchdog standing down: this node's pods do not arrive through cilium-cni, "+
					"so a pod without a local Cilium endpoint is not ours to heal",
				logfields.Reason, reason,
			)
		}
		return nil
	}

	txn := w.db.ReadTxn()
	now := w.now()
	seen := make(map[k8stypes.UID]struct{})

	// eligible = pods that could have a local endpoint; healthy = those that
	// do. due = those past the grace period, healed only if the guard below
	// agrees the premise holds.
	var (
		eligible int
		healthy  int
		due      []tables.LocalPod
	)

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
		eligible++
		if w.endpoints.LookupIP(addr) != nil {
			healthy++
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
			due = append(due, pod)
		}
	}

	// BLAST RADIUS GUARD, deliberately independent of the ownership gate.
	//
	// The gate was wrong once (a conflist that existed but was not in use) and
	// can be wrong again: a stale node label, an unlabelled node, perigeos
	// crashing between switching backends and updating the label. Every one of
	// those ends in the same place unless something bounds the damage.
	//
	// If NOT ONE pod on this node has an endpoint, that is not N broken pods,
	// it is one broken assumption -- either these pods are not ours, or the
	// endpoint manager itself is empty. Deleting every workload on the node is
	// the wrong response to both. A genuine post-restart endpoint loss leaves
	// some endpoints standing; engifire had none, and lost four workloads plus
	// a probe in three minutes.
	//
	// Not latched: this is an alarm, not a steady state, and it must keep
	// saying so.
	if limit := blastRadiusLimit(eligible); len(due) > limit {
		w.logger.Error(
			"Pod-endpoint watchdog refusing to act: too many pods are missing their local Cilium "+
				"endpoint at once, which indicates a wrong premise (pods not ours, or endpoint "+
				"state missing) rather than each pod being individually broken",
			logfields.Count, len(due),
			logfields.Limit, limit,
			logfields.Total, eligible,
		)
		return nil
	}

	for _, pod := range due {
		w.heal(ctx, pod)
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

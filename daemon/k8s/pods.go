// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package k8s

import (
	"context"
	"fmt"
	"iter"
	"strconv"
	"strings"

	"github.com/cilium/hive/cell"
	"github.com/cilium/hive/job"
	"github.com/cilium/statedb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/cilium/cilium/pkg/k8s"
	"github.com/cilium/cilium/pkg/k8s/client"
	slim_corev1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/core/v1"
	"github.com/cilium/cilium/pkg/k8s/utils"
	nodeTypes "github.com/cilium/cilium/pkg/node/types"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/time"
)

// LocalPod is Cilium's internal model of the pods running on this node.
type LocalPod struct {
	*slim_corev1.Pod

	// UpdatedAt is the time when [LocalPod] was last updated, e.g. it
	// shows when the pod change was received from the api-server.
	UpdatedAt time.Time `json:"updatedAt" yaml:"updatedAt"`
}

func (p LocalPod) TableHeader() []string {
	return []string{
		"Name",
		"UID",
		"HostNetwork",
		"PodIPs",
		"Containers",
		"Phase",
		"Age",
	}
}

func (p LocalPod) TableRow() []string {
	podIPs := make([]string, len(p.Status.PodIPs))
	for i := range p.Status.PodIPs {
		podIPs[i] = p.Status.PodIPs[i].IP
	}
	containers := make([]string, len(p.Spec.Containers))
	for i, cont := range p.Spec.Containers {
		ports := make([]string, len(cont.Ports))
		for i, port := range cont.Ports {
			if port.Name != "" {
				ports[i] = fmt.Sprintf("%d/%s (%s)", port.ContainerPort, string(port.Protocol), port.Name)
			} else {
				ports[i] = fmt.Sprintf("%d/%s", port.ContainerPort, string(port.Protocol))
			}
		}
		contName := cont.Name
		if len(ports) > 0 {
			contName += " (" + strings.Join(ports, ",") + ")"
		}
		containers[i] = contName
	}
	return []string{
		p.Namespace + "/" + p.Name,
		string(p.UID),
		strconv.FormatBool(p.Spec.HostNetwork),
		strings.Join(podIPs, ", "),
		strings.Join(containers, ", "),
		string(p.Status.Phase),
		duration.HumanDuration(time.Since(p.UpdatedAt)),
	}
}

const (
	PodTableName = "k8s-pods"
)

var (
	PodNameIndex = newNameIndex[LocalPod]()
	PodTableCell = cell.Provide(NewPodTableAndReflector)
)

// NewPodTableAndReflector returns the read-only Table[LocalPod] and registers
// k8s reflectors for all managed node names. These are combined to ensure any
// dependency on Table[LocalPod] will start after all reflectors, ensuring that
// Start hooks can wait for the table to initialize.
//
// In standard Cilium deployments there is one managed node (the local host)
// and therefore one reflector, matching upstream behaviour exactly.
// In Constellation/perigeos host-sharding mode there are N managed nodes
// (one per pawn); each gets its own field-selector watch, all writing into
// the same table.
func NewPodTableAndReflector(jg job.Group, db *statedb.DB, cs client.Clientset) (statedb.Table[LocalPod], error) {
	pods, err := NewPodTable(db)
	if err != nil {
		return nil, err
	}

	if !cs.IsEnabled() {
		return pods, nil
	}

	selector := option.Config.ManagedNodesSelector
	if selector != "" {
		// Label-selector mode: discover nodes matching the selector,
		// create a pod reflector per node, and start a background
		// watcher for dynamic node addition/removal.
		ctx := context.TODO()
		names, err := discoverManagedNodes(ctx, cs, selector)
		if err != nil {
			return nil, err
		}

		if len(names) == 0 {
			// No matching nodes yet — fall back to local node so the
			// agent can still manage its own host pods.
			names = []string{nodeTypes.GetName()}
			nodeTypes.SetManagedNames(names)
		}

		for _, name := range names {
			cfg := podReflectorConfig(cs, pods, name)
			if err := k8s.RegisterReflector(jg, db, cfg); err != nil {
				return nil, fmt.Errorf("registering pod reflector for node %q: %w", name, err)
			}
		}

		startNodeWatcher(jg, db, cs, pods, selector, names)
		return pods, nil
	}

	// Standard single-node mode — one reflector for the local node.
	for _, nodeName := range nodeTypes.GetManagedNames() {
		cfg := podReflectorConfig(cs, pods, nodeName)
		if err := k8s.RegisterReflector(jg, db, cfg); err != nil {
			return nil, fmt.Errorf("registering pod reflector for node %q: %w", nodeName, err)
		}
	}
	return pods, nil
}

func PodByName(namespace, name string) statedb.Query[LocalPod] {
	return PodNameIndex.Query(namespace + "/" + name)
}

func NewPodTable(db *statedb.DB) (statedb.RWTable[LocalPod], error) {
	return statedb.NewTable(
		db,
		PodTableName,
		PodNameIndex,
	)
}

// podReflectorConfig builds a ReflectorConfig that watches pods on nodeName
// and writes them into the shared pods table. Each call produces a reflector
// with a unique Name so the statedb initializer tracking works correctly when
// multiple reflectors share the same table.
func podReflectorConfig(cs client.Clientset, pods statedb.RWTable[LocalPod], nodeName string) k8s.ReflectorConfig[LocalPod] {
	lw := utils.ListerWatcherWithModifiers(
		utils.ListerWatcherFromTyped(cs.Slim().CoreV1().Pods("")),
		func(opts *metav1.ListOptions) {
			opts.FieldSelector = fields.ParseSelectorOrDie("spec.nodeName=" + nodeName).String()
		})
	name := reflectorName
	if nodeName != nodeTypes.GetName() {
		// Append the node name so each reflector has a unique name in the
		// statedb initializer registry. The primary node keeps the canonical
		// name for compatibility with any code that waits on it by name.
		name = reflectorName + "/" + nodeName
	}
	return k8s.ReflectorConfig[LocalPod]{
		Name:          name,
		Table:         pods,
		ListerWatcher: lw,
		MetricScope:   "Pod",
		// QueryAll must be scoped to this reflector's node so that a Replace
		// (initial list or resync) from one reflector does not delete entries
		// written by another reflector sharing the same table.
		// Without this, the default queryAll = tbl.All() causes reflector N's
		// Replace to wipe out everything reflectors 1..N-1 just wrote.
		QueryAll: func(txn statedb.ReadTxn, tbl statedb.Table[LocalPod]) iter.Seq2[LocalPod, statedb.Revision] {
			return func(yield func(LocalPod, statedb.Revision) bool) {
				for pod, rev := range tbl.All(txn) {
					if pod.Spec.NodeName == nodeName {
						if !yield(pod, rev) {
							return
						}
					}
				}
			}
		},
		Transform: func(_ statedb.ReadTxn, obj any) (LocalPod, bool) {
			pod, ok := obj.(*slim_corev1.Pod)
			if !ok {
				return LocalPod{}, false
			}
			return LocalPod{
				Pod:       pod,
				UpdatedAt: time.Now(),
			}, true
		},
	}
}

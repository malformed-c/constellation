// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Constellation

package k8s

// Node watcher for --managed-nodes-selector.
//
// When the agent is configured with a label selector (e.g.
// perigeos.io/host=engix99), this module discovers the k8s Node objects
// that match and wires them into the managed-names set used by the pod
// reflectors and endpoint restore logic.
//
// At startup, discoverManagedNodes performs a synchronous List to seed the
// initial set. A background watch (watchManagedNodes) then handles dynamic
// node addition/removal — e.g. when perigeos registers a new pawn.

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/cilium/hive/cell"
	"github.com/cilium/hive/job"
	"github.com/cilium/statedb"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/watch"

	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	"github.com/cilium/cilium/pkg/k8s"
	"github.com/cilium/cilium/pkg/k8s/client"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/node/addressing"
	nodeTypes "github.com/cilium/cilium/pkg/node/types"
	"github.com/cilium/cilium/pkg/time"
)

var nodeWatcherLog = logging.DefaultSlogLogger.With(logfields.LogSubsys, "node-watcher")

// NodeAddedCallback is called when a new managed node is discovered by the
// watcher. Used by IPAM to dynamically add per-node sub-allocators.
type NodeAddedCallback func(nodeName string)

var (
	nodeAddedCallbackMu sync.Mutex
	nodeAddedCallbacks  []NodeAddedCallback
)

// RegisterNodeAddedCallback registers a callback that will be invoked
// whenever a new managed node is discovered by the node watcher.
func RegisterNodeAddedCallback(cb NodeAddedCallback) {
	nodeAddedCallbackMu.Lock()
	defer nodeAddedCallbackMu.Unlock()
	nodeAddedCallbacks = append(nodeAddedCallbacks, cb)
}

// ResetNodeAddedCallbacks clears all registered callbacks. For testing only.
func ResetNodeAddedCallbacks() {
	nodeAddedCallbackMu.Lock()
	defer nodeAddedCallbackMu.Unlock()
	nodeAddedCallbacks = nil
}

func fireNodeAdded(name string) {
	nodeAddedCallbackMu.Lock()
	cbs := make([]NodeAddedCallback, len(nodeAddedCallbacks))
	copy(cbs, nodeAddedCallbacks)
	nodeAddedCallbackMu.Unlock()

	for _, cb := range cbs {
		cb(name)
	}
}

// discoverManagedNodes lists k8s Node objects matching selector and returns
// their names. It also calls nodeTypes.SetManagedNames so that IsManaged()
// works immediately after this function returns.
//
// Returns an empty slice (not an error) if no nodes match — the caller
// should fall back to standard single-node behaviour.
func discoverManagedNodes(ctx context.Context, cs client.Clientset, selector string) ([]string, error) {
	nodeList, err := cs.Slim().CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("listing nodes with selector %q: %w", selector, err)
	}

	names := make([]string, 0, len(nodeList.Items))
	for i := range nodeList.Items {
		names = append(names, nodeList.Items[i].Name)
	}

	if len(names) > 0 {
		nodeTypes.SetManagedNames(names)
	}

	// Ensure CiliumNode resources exist for all managed nodes so the
	// operator allocates per-node CIDRs before IPAM initialization.
	localName := nodeTypes.GetName()
	for _, name := range names {
		if name == localName {
			continue
		}
		ensureCiliumNodeFromAPI(ctx, cs, name)
	}

	nodeWatcherLog.Info("Discovered managed nodes",
		logfields.Selector, selector,
		logfields.Count, len(names),
		logfields.Node, names)
	return names, nil
}

// ensureCiliumNodeFromAPI creates a CiliumNode if it doesn't exist, fetching
// the InternalIP from the k8s Node API.
func ensureCiliumNodeFromAPI(ctx context.Context, cs client.Clientset, name string) {
	// Check if CiliumNode already exists.
	_, err := cs.CiliumV2().CiliumNodes().Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return
	}

	node, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		nodeWatcherLog.Warn("Could not fetch Node for CiliumNode creation",
			logfields.Node, name,
			logfields.Error, err,
		)
		return
	}

	var nodeIP string
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			nodeIP = addr.Address
			break
		}
	}
	if nodeIP == "" {
		return
	}

	cn := &ciliumv2.CiliumNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: ciliumv2.NodeSpec{
			Addresses: []ciliumv2.NodeAddress{
				{Type: addressing.NodeInternalIP, IP: nodeIP},
			},
			HealthAddressing: ciliumv2.HealthAddressingSpec{
				IPv4: nodeIP,
			},
			Encryption: ciliumv2.EncryptionSpec{
				Key: 0,
			},
		},
	}

	_, err = cs.CiliumV2().CiliumNodes().Create(ctx, cn, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		nodeWatcherLog.Warn("Failed to create CiliumNode for managed node",
			logfields.Node, name,
			logfields.Error, err,
		)
		return
	}
	nodeWatcherLog.Info("Created CiliumNode for managed node",
		logfields.Node, name,
	)
}

// nodeWatcher watches Node objects matching a label selector and
// dynamically updates the managed names set and pod reflectors.
type nodeWatcher struct {
	logger         *slog.Logger
	cs             client.Clientset
	selector       string
	parsedSelector labels.Selector

	// For registering new pod reflectors when nodes are added.
	jg   job.Group
	db   *statedb.DB
	pods statedb.RWTable[LocalPod]

	// Track known nodes to detect additions/removals.
	mu    sync.Mutex
	known map[string]struct{}
}

// startNodeWatcher registers a background job that watches for Node
// add/delete events matching the selector. When new nodes appear, it
// registers a pod reflector for them and updates nodeTypes.SetManagedNames.
func startNodeWatcher(
	jg job.Group,
	db *statedb.DB,
	cs client.Clientset,
	pods statedb.RWTable[LocalPod],
	selector string,
	initialNames []string,
) {
	known := make(map[string]struct{}, len(initialNames))
	for _, n := range initialNames {
		known[n] = struct{}{}
	}

	parsed, err := labels.Parse(selector)
	if err != nil {
		// Selector was already validated by discoverManagedNodes List call.
		parsed = labels.Nothing()
	}

	nw := &nodeWatcher{
		logger:         nodeWatcherLog,
		cs:             cs,
		selector:       selector,
		parsedSelector: parsed,
		jg:             jg,
		db:             db,
		pods:           pods,
		known:          known,
	}

	jg.Add(job.OneShot("managed-node-watcher", nw.run))
}

func (nw *nodeWatcher) run(ctx context.Context, health cell.Health) error {
	for ctx.Err() == nil {
		if err := nw.watch(ctx); err != nil {
			nw.logger.Warn("Node watch error, retrying",
				logfields.Error, err)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}
	return nil
}

func (nw *nodeWatcher) watch(ctx context.Context) error {
	watcher, err := nw.cs.Slim().CoreV1().Nodes().Watch(ctx, metav1.ListOptions{
		LabelSelector: nw.selector,
	})
	if err != nil {
		return fmt.Errorf("starting node watch: %w", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case evt, ok := <-watcher.ResultChan():
			if !ok {
				return fmt.Errorf("node watch channel closed")
			}
			nw.handleEvent(evt)
		}
	}
}

func (nw *nodeWatcher) handleEvent(evt watch.Event) {
	switch evt.Type {
	case watch.Added:
		nw.handleAdded(evt)
	case watch.Deleted:
		nw.handleDeleted(evt)
	}
}

type labeledObject interface {
	GetName() string
	GetLabels() map[string]string
}

func (nw *nodeWatcher) handleAdded(evt watch.Event) {
	obj, ok := evt.Object.(labeledObject)
	if !ok {
		return
	}
	// Validate that the node actually matches our selector. The API server
	// filters watch events by label selector, but this guards against
	// edge cases and test fakes that may not filter.
	if !nw.parsedSelector.Matches(labels.Set(obj.GetLabels())) {
		return
	}
	name := obj.GetName()

	nw.mu.Lock()
	_, alreadyKnown := nw.known[name]
	nw.known[name] = struct{}{}

	if !alreadyKnown {
		// Update managed names.
		names := nw.knownNames()
		nodeTypes.SetManagedNames(names)

		// Register a new pod reflector for this node.
		cfg := podReflectorConfig(nw.cs, nw.pods, name)
		if err := k8s.RegisterReflector(nw.jg, nw.db, cfg); err != nil {
			nw.logger.Error("Failed to register pod reflector for new node",
				logfields.Node, name,
				logfields.Error, err,
			)
			nw.mu.Unlock()
			return
		}

		nw.logger.Info("Discovered new managed node, started pod reflector",
			logfields.Node, name,
			logfields.Total, len(names),
		)
	}
	nw.mu.Unlock()

	// Ensure a CiliumNode exists for this managed node so the operator
	// allocates a per-node CIDR.
	go nw.ensureCiliumNodeForManagedNode(name)

	// Always notify callbacks — even for known nodes. On watch reconnect,
	// Added events fire for all existing nodes. IPAM uses this to detect
	// CiliumNode CIDR changes (e.g. after CiliumNode deletion/recreation).
	fireNodeAdded(name)
}

func (nw *nodeWatcher) handleDeleted(evt watch.Event) {
	obj, ok := evt.Object.(labeledObject)
	if !ok {
		return
	}
	name := obj.GetName()

	nw.mu.Lock()
	defer nw.mu.Unlock()

	if _, exists := nw.known[name]; !exists {
		return
	}
	delete(nw.known, name)

	// Update managed names (the pod reflector for this node stays
	// registered but will watch nothing — its field selector won't
	// match any pods on a deleted node).
	names := nw.knownNames()
	nodeTypes.SetManagedNames(names)

	nw.logger.Info("Managed node removed",
		logfields.Node, name,
		logfields.Remaining, len(names),
	)
}

// knownNames returns a sorted slice of known node names. Must be called
// with nw.mu held.
func (nw *nodeWatcher) knownNames() []string {
	names := make([]string, 0, len(nw.known))
	for n := range nw.known {
		names = append(names, n)
	}
	return names
}

// ensureCiliumNodeForManagedNode creates a CiliumNode resource for a managed
// node if one doesn't already exist.
func (nw *nodeWatcher) ensureCiliumNodeForManagedNode(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ensureCiliumNodeFromAPI(ctx, nw.cs, name)
}

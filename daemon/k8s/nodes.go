// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Constellation

package k8s

// Node watcher for --managed-node-selector.
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/cilium/cilium/pkg/k8s"
	"github.com/cilium/cilium/pkg/k8s/client"
	nodeTypes "github.com/cilium/cilium/pkg/node/types"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/time"
)

var nodeWatcherLog = logging.DefaultSlogLogger.With(logfields.LogSubsys, "node-watcher")

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

	nodeWatcherLog.Info("Discovered managed nodes",
		"selector", selector,
		"count", len(names),
		"nodes", names)
	return names, nil
}

// nodeWatcher watches Node objects matching a label selector and
// dynamically updates the managed names set and pod reflectors.
type nodeWatcher struct {
	logger   *slog.Logger
	cs       client.Clientset
	selector string

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

	nw := &nodeWatcher{
		logger:   nodeWatcherLog,
		cs:       cs,
		selector: selector,
		jg:       jg,
		db:       db,
		pods:     pods,
		known:    known,
	}

	jg.Add(job.OneShot("managed-node-watcher", nw.run))
}

func (nw *nodeWatcher) run(ctx context.Context, health cell.Health) error {
	for {
		if err := nw.watch(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			nw.logger.Warn("Node watch error, retrying",
				logfields.Error, err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(5 * time.Second):
			}
		}
	}
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

type namedObject interface {
	GetName() string
}

func (nw *nodeWatcher) handleAdded(evt watch.Event) {
	obj, ok := evt.Object.(namedObject)
	if !ok {
		return
	}
	name := obj.GetName()

	nw.mu.Lock()
	defer nw.mu.Unlock()

	if _, exists := nw.known[name]; exists {
		return // already tracking
	}
	nw.known[name] = struct{}{}

	// Update managed names.
	names := nw.knownNames()
	nodeTypes.SetManagedNames(names)

	// Register a new pod reflector for this node.
	cfg := podReflectorConfig(nw.cs, nw.pods, name)
	if err := k8s.RegisterReflector(nw.jg, nw.db, cfg); err != nil {
		nw.logger.Error("Failed to register pod reflector for new node",
			"node", name, logfields.Error, err)
		return
	}

	nw.logger.Info("Discovered new managed node, started pod reflector",
		"node", name, "total", len(names))
}

func (nw *nodeWatcher) handleDeleted(evt watch.Event) {
	obj, ok := evt.Object.(namedObject)
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
		"node", name, "remaining", len(names))
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

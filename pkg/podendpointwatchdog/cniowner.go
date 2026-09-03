// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Constellation

package podendpointwatchdog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ciliumCNIPluginName is the plugin type perigeos writes into the conflist
// when a node's pod networking is handed to this agent.
const ciliumCNIPluginName = "cilium-cni"

// DefaultCNIConfDir is where perigeos writes constellation's CNI conflist. The
// agent already mounts exactly this path (see the chart's cni-conf-dir volume),
// so the check needs no new mount.
const DefaultCNIConfDir = "/etc/cni/net.d/constellation"

// cniOwnership reports whether this node's pods are supposed to arrive through
// cilium-cni, plus a human-readable reason for the answer. The reason is what
// makes a stand-down diagnosable rather than a silent no-op.
type cniOwnership func(ctx context.Context) (bool, string)

// nodeUsesCiliumCNI reports whether confDir contains a CNI configuration naming
// cilium-cni as a plugin.
//
// This is the watchdog's authority to act at all. Without it the watchdog reads
// "Running pod with an IP and no local Cilium endpoint" as "broken, delete it" —
// which is true only on a node whose pods come through us. On a node still
// running another CNI (kube-router/bridge, during a migration) NO pod has a
// Cilium endpoint, so every pod-network pod on the node matches, and each
// replacement comes back through the same other CNI and matches again. That is
// not healing, it is an unbounded delete loop against another CNI's workloads.
// Observed 2026-09-03 on engifire: four workloads plus a probe deleted within
// three minutes of the DaemonSet landing, then looping.
//
// A pod that arrived through another CNI is not missing an endpoint. It is not
// ours, and the correct action is none.
//
// Errors are reported as NOT owned. Refusing to act when we cannot establish
// ownership is the safe direction: the cost is an unhealed pod, versus deleting
// another CNI's entire workload set.
func nodeUsesCiliumCNI(confDir string) (bool, string) {
	entries, err := os.ReadDir(confDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, fmt.Sprintf("CNI config directory %s does not exist", confDir)
		}
		return false, fmt.Sprintf("cannot read CNI config directory %s: %v", confDir, err)
	}

	// Sorted only so the reported reason is stable across runs. Do NOT read
	// this as "the lexically first config wins": that is how a runtime picks
	// among configs in ONE directory, and perigeos does not do that -- it
	// selects a PROVIDER DIRECTORY from its own config and points the runtime
	// at it. Inside that directory the question is simply whether a cilium
	// configuration is present, so every file is checked.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ext := filepath.Ext(e.Name()); ext == ".conf" || ext == ".conflist" || ext == ".json" {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)

	if len(names) == 0 {
		return false, fmt.Sprintf("no CNI configuration in %s", confDir)
	}

	var seen []string
	for _, name := range names {
		path := filepath.Join(confDir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		types, err := cniPluginTypes(raw)
		if err != nil {
			continue
		}
		if slices.Contains(types, ciliumCNIPluginName) {
			return true, fmt.Sprintf("%s declares %s", path, ciliumCNIPluginName)
		}
		seen = append(seen, fmt.Sprintf("%s=%s", name, strings.Join(types, ",")))
	}

	if len(seen) == 0 {
		return false, fmt.Sprintf("no readable CNI configuration in %s", confDir)
	}
	return false, fmt.Sprintf("no configuration in %s declares %s (found %s)",
		confDir, ciliumCNIPluginName, strings.Join(seen, " "))
}

// cniPluginTypes extracts the plugin type(s) from either a .conflist (a
// "plugins" array) or a single-plugin .conf (a top-level "type").
func cniPluginTypes(raw []byte) ([]string, error) {
	var doc struct {
		Type    string `json:"type"`
		Plugins []struct {
			Type string `json:"type"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}

	var types []string
	if doc.Type != "" {
		types = append(types, doc.Type)
	}
	for _, p := range doc.Plugins {
		if p.Type != "" {
			types = append(types, p.Type)
		}
	}
	if len(types) == 0 {
		return nil, fmt.Errorf("no plugin type found")
	}
	return types, nil
}

// CNIProviderLabel is the node label perigeos publishes to declare which CNI
// backend it is ACTUALLY routing pods through on that host. Values are
// constellation | standard | builtin | pending, where pending means perigeos
// is waiting for a CNI and routing nothing -- deliberately distinct from
// standard, and equally not ours.
//
// perigeos DERIVES this from the live backend on every node-status cycle
// rather than writing it at switch time, so the label cannot lead the backend
// and there is no write order to get wrong. A promotion therefore shows up
// within one cycle; this gate re-reads every scan to match.
//
// This, not the presence of a config file, is the watchdog's authority. A
// conflist sitting in a provider subdirectory says only that perigeos once
// wrote it; on engifire a constellation.conflist sat unused under another
// backend, so a file-presence check answers "yes" on precisely the node where
// acting is catastrophic. Which backend is live is a fact perigeos knows and
// nobody else can derive.
const CNIProviderLabel = "peri.apsis/cni-provider"

// The CNIProviderLabel values perigeos publishes. Only constellation makes a
// node's pods ours to heal; the rest are recorded so that a value outside the
// contract can be told apart from one inside it.
const (
	CNIProviderConstellation = "constellation"
	CNIProviderStandard      = "standard"
	CNIProviderBuiltin       = "builtin"
	CNIProviderPending       = "pending"
)

// knownCNIProviders is the agreed value set. Membership changes nothing about
// whether we act -- anything that is not constellation is not ours either way
// -- but it changes what the log says, and that is the difference between "this
// node runs another CNI, as expected" and "perigeos is emitting a value this
// build has never heard of". The second is a contract drift someone needs to
// see; without this it reads identically to the first.
var knownCNIProviders = map[string]struct{}{
	CNIProviderConstellation: {},
	CNIProviderStandard:      {},
	CNIProviderBuiltin:       {},
	CNIProviderPending:       {},
}

// nodeLabelSaysCilium reports whether the local node declares constellation as
// its live CNI backend.
//
// Absent, empty, or any other value means NOT ours. That covers a node running
// another backend, a node perigeos has not labelled yet, and a node whose
// perigeos predates the contract - all of which must be left alone.
func nodeLabelSaysCilium(labels map[string]string) (bool, string) {
	v, ok := labels[CNIProviderLabel]
	switch {
	case !ok:
		return false, fmt.Sprintf("node label %s is absent", CNIProviderLabel)
	case v == CNIProviderConstellation:
		return true, fmt.Sprintf("node label %s=%s", CNIProviderLabel, v)
	default:
		if _, known := knownCNIProviders[v]; known {
			return false, fmt.Sprintf("node label %s=%s, not %s",
				CNIProviderLabel, v, CNIProviderConstellation)
		}
		// Still not ours -- unrecognised is never a reason to act -- but say
		// so differently, because this one means the contract has moved.
		return false, fmt.Sprintf(
			"node label %s=%q is not a recognised CNI provider value (expected one of %s); "+
				"treating as not ours",
			CNIProviderLabel, v, strings.Join(sortedProviders(), ", "))
	}
}

// sortedProviders lists the agreed values in a stable order for log messages.
func sortedProviders() []string {
	out := make([]string, 0, len(knownCNIProviders))
	for k := range knownCNIProviders {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

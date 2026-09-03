// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Constellation

package podendpointwatchdog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeConf(t *testing.T, dir, name, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
}

const ciliumConflist = `{
  "cniVersion": "0.3.1",
  "name": "constellation",
  "plugins": [{"cniVersion": "0.3.1", "name": "constellation", "type": "cilium-cni"}]
}`

// The bridge/kube-router shape that was live on engifire on 2026-09-03, when
// the watchdog deleted four workloads plus a probe and then looped.
const bridgeConflist = `{
  "cniVersion": "0.3.1",
  "name": "kube-router",
  "plugins": [{"type": "bridge", "bridge": "kube-bridge", "isDefaultGateway": true}]
}`

func TestNodeUsesCiliumCNI(t *testing.T) {
	t.Run("cilium conflist: owned", func(t *testing.T) {
		dir := t.TempDir()
		writeConf(t, dir, "constellation.conflist", ciliumConflist)
		owned, reason := nodeUsesCiliumCNI(dir)
		require.True(t, owned, reason)
	})

	t.Run("another CNI: not owned", func(t *testing.T) {
		dir := t.TempDir()
		writeConf(t, dir, "10-kube-router.conflist", bridgeConflist)
		owned, reason := nodeUsesCiliumCNI(dir)
		require.False(t, owned, "a bridge CNI node's pods are not ours")
		require.Contains(t, reason, "bridge")
	})

	t.Run("single-plugin .conf form is understood too", func(t *testing.T) {
		dir := t.TempDir()
		writeConf(t, dir, "10-cilium.conf", `{"cniVersion":"0.3.1","type":"cilium-cni"}`)
		owned, _ := nodeUsesCiliumCNI(dir)
		require.True(t, owned)
	})

	// CNI picks the lexically first configuration. A leftover cilium file
	// sitting behind another CNI's active one must not read as ownership.
	t.Run("lexically first config wins", func(t *testing.T) {
		dir := t.TempDir()
		writeConf(t, dir, "05-kube-router.conflist", bridgeConflist)
		writeConf(t, dir, "99-constellation.conflist", ciliumConflist)
		owned, reason := nodeUsesCiliumCNI(dir)
		require.False(t, owned,
			"a stale cilium conflist behind another CNI's active one is not ownership")
		require.Contains(t, reason, "05-kube-router")
	})

	// Not establishing ownership must read as "not ours": the cost of a wrong
	// false is one unhealed pod, the cost of a wrong true is deleting another
	// CNI's entire workload set in a loop.
	t.Run("missing directory: not owned", func(t *testing.T) {
		owned, reason := nodeUsesCiliumCNI(filepath.Join(t.TempDir(), "absent"))
		require.False(t, owned)
		require.Contains(t, reason, "does not exist")
	})

	t.Run("empty directory: not owned", func(t *testing.T) {
		owned, _ := nodeUsesCiliumCNI(t.TempDir())
		require.False(t, owned)
	})

	t.Run("unparseable config: not owned", func(t *testing.T) {
		dir := t.TempDir()
		writeConf(t, dir, "broken.conflist", "{not json")
		owned, _ := nodeUsesCiliumCNI(dir)
		require.False(t, owned)
	})
}

// The primary authority: what perigeos says it is ACTUALLY running.
//
// Everything that is not exactly "constellation" must read as not ours -- that
// is what covers a node on another backend, a node perigeos has not labelled
// yet, and a node whose perigeos predates this contract.
func TestNodeLabelSaysCilium(t *testing.T) {
	for name, tc := range map[string]struct {
		labels map[string]string
		want   bool
	}{
		"constellation": {map[string]string{CNIProviderLabel: "constellation"}, true},
		"standard":      {map[string]string{CNIProviderLabel: "standard"}, false},
		"builtin":       {map[string]string{CNIProviderLabel: "builtin"}, false},
		// pending = perigeos is waiting for a CNI and routing nothing. A real
		// documented value, not an unknown one, so it is asserted by name.
		"pending":           {map[string]string{CNIProviderLabel: "pending"}, false},
		"absent":            {map[string]string{"peri.apsis/host": "engix99"}, false},
		"empty value":       {map[string]string{CNIProviderLabel: ""}, false},
		"nil labels":        {nil, false},
		"unknown value":     {map[string]string{CNIProviderLabel: "something-new"}, false},
		"case must match":   {map[string]string{CNIProviderLabel: "Constellation"}, false},
		"no leading spaces": {map[string]string{CNIProviderLabel: " constellation"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, reason := nodeLabelSaysCilium(tc.labels)
			require.Equal(t, tc.want, got, reason)
			require.NotEmpty(t, reason, "the reason is what makes a stand-down diagnosable")
		})
	}
}

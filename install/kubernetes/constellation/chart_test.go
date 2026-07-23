// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

// Package constellation_test exercises the constellation Helm chart's
// rendered output. It's pure YAML, not Go source, so normal Go unit tests
// don't cover it otherwise.
package constellation_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	yaml "go.yaml.in/yaml/v3"
)

// renderAgentDaemonSet runs `helm template` against this chart and returns
// the constellation-agent DaemonSet's pod spec as a generic map, so tests
// can assert on the rendered structure without a full typed k8s dependency.
func renderAgentDaemonSet(t *testing.T, extraSet ...string) map[string]any {
	t.Helper()

	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not available")
	}

	args := []string{
		"template", "constellation", ".",
		"--set", "k8sServiceHost=192.168.50.1",
		"--set", "k8sServicePort=6443",
	}
	args = append(args, extraSet...)

	cmd := exec.Command("helm", args...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "helm template failed: %s", out)

	dec := yaml.NewDecoder(strings.NewReader(string(out)))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc["kind"] == "DaemonSet" {
			meta, _ := doc["metadata"].(map[string]any)
			if meta["name"] == "constellation-agent" {
				spec := doc["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
				return spec
			}
		}
	}
	t.Fatal("constellation-agent DaemonSet not found in rendered output")
	return nil
}

func findByName(t *testing.T, list []any, name string) map[string]any {
	t.Helper()
	for _, item := range list {
		m := item.(map[string]any)
		if m["name"] == name {
			return m
		}
	}
	t.Fatalf("%q not found", name)
	return nil
}

func hasByName(list []any, name string) bool {
	for _, item := range list {
		if m, ok := item.(map[string]any); ok && m["name"] == name {
			return true
		}
	}
	return false
}

func toStringSlice(v any) []string {
	list, _ := v.([]any)
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = s.(string)
	}
	return out
}

// TestAgentDaemonSet_NoPerNodeServiceHostDetection is a regression test for
// the removal of the resolve-k8s-service-host initContainer/wrapper-script
// mechanism: per-node loopback detection was replaced by Cilium's own
// built-in --k8s-api-server-urls multi-candidate failover (see
// TestAgentDaemonSet_K8sAPIServerURLs), which needs no shell script, no
// shared emptyDir, and no wrapped command. This checks that machinery
// actually stays gone rather than silently reappearing.
func TestAgentDaemonSet_NoPerNodeServiceHostDetection(t *testing.T) {
	spec := renderAgentDaemonSet(t)

	initContainers := spec["initContainers"].([]any)
	require.False(t, hasByName(initContainers, "resolve-k8s-service-host"))
	require.Len(t, initContainers, 2, "only fix-sysctls and install-cni should remain")

	containers := spec["containers"].([]any)
	agent := findByName(t, containers, "agent")
	require.Equal(t, []any{"/usr/bin/cilium-agent"}, agent["command"],
		"main container must run cilium-agent directly, no wrapper script")

	agentMounts, _ := agent["volumeMounts"].([]any)
	require.False(t, hasByName(agentMounts, "k8s-service-host"))

	volumes := spec["volumes"].([]any)
	require.False(t, hasByName(volumes, "k8s-service-host"))
}

// TestAgentDaemonSet_K8sAPIServerURLsEmpty verifies the default (single
// static k8sServiceHost, no k8sAPIServerURLs) path still renders the plain
// KUBERNETES_SERVICE_HOST/PORT env vars and no --k8s-api-server-urls args -
// existing simple/single-CP-address deployments are unaffected.
func TestAgentDaemonSet_K8sAPIServerURLsEmpty(t *testing.T) {
	spec := renderAgentDaemonSet(t)
	agent := findByName(t, spec["containers"].([]any), "agent")

	args := toStringSlice(agent["args"])
	for _, a := range args {
		require.NotContains(t, a, "--k8s-api-server-urls",
			"no --k8s-api-server-urls arg should render when k8sAPIServerURLs is unset")
	}

	env := agent["env"].([]any)
	hostEnv := findByName(t, env, "KUBERNETES_SERVICE_HOST")
	require.Equal(t, "192.168.50.1", hostEnv["value"])
	portEnv := findByName(t, env, "KUBERNETES_SERVICE_PORT")
	require.Equal(t, "6443", portEnv["value"])
}

// TestAgentDaemonSet_K8sAPIServerURLs verifies that setting the
// k8sAPIServerURLs list renders one --k8s-api-server-urls arg per entry, in
// order, alongside (not instead of) the existing static env vars - Cilium's
// restConfigManager already knows how to pick among multiple candidates and
// retry on failure, so the chart only needs to pass them through.
func TestAgentDaemonSet_K8sAPIServerURLs(t *testing.T) {
	spec := renderAgentDaemonSet(t,
		"--set", "k8sAPIServerURLs[0]=https://192.168.100.200:6443",
		"--set", "k8sAPIServerURLs[1]=https://192.168.50.1:6443",
	)
	agent := findByName(t, spec["containers"].([]any), "agent")
	args := toStringSlice(agent["args"])

	require.Contains(t, args, "--k8s-api-server-urls=https://192.168.100.200:6443")
	require.Contains(t, args, "--k8s-api-server-urls=https://192.168.50.1:6443")

	i0 := indexOf(args, "--k8s-api-server-urls=https://192.168.100.200:6443")
	i1 := indexOf(args, "--k8s-api-server-urls=https://192.168.50.1:6443")
	require.Less(t, i0, i1, "URLs must render in the order given")

	// Still present as the InClusterConfig fallback if every configured
	// URL is ever unreachable.
	env := agent["env"].([]any)
	hostEnv := findByName(t, env, "KUBERNETES_SERVICE_HOST")
	require.Equal(t, "192.168.50.1", hostEnv["value"])
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

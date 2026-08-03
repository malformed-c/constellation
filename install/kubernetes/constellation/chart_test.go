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

// TestAgentDaemonSet_K8sServiceHostRequirement covers constellation.k8sServiceHost's
// three cases: neither value set must fail the render (no way to reach the API
// server at all); k8sServiceHost set is unaffected (existing behavior); and
// k8sAPIServerURLs set without k8sServiceHost must NOT fail, but still needs a
// non-empty KUBERNETES_SERVICE_HOST placeholder - createConfig's
// k8sAPIServerURLs path calls rest.InClusterConfig() first (for the pod's
// token/CA) before overriding .Host, and InClusterConfig() rejects an empty
// KUBERNETES_SERVICE_HOST outright, so a genuinely empty value would break
// bootstrap even with the URL list configured.
func TestAgentDaemonSet_K8sServiceHostRequirement(t *testing.T) {
	t.Run("neither set fails", func(t *testing.T) {
		if _, err := exec.LookPath("helm"); err != nil {
			t.Skip("helm not available")
		}
		cmd := exec.Command("helm", "template", "constellation", ".")
		out, err := cmd.CombinedOutput()
		require.Error(t, err)
		require.Contains(t, string(out), "k8sServiceHost is required")
	})

	t.Run("k8sAPIServerURLs set without k8sServiceHost succeeds with a placeholder", func(t *testing.T) {
		if _, err := exec.LookPath("helm"); err != nil {
			t.Skip("helm not available")
		}
		cmd := exec.Command("helm", "template", "constellation", ".",
			"--set", "k8sAPIServerURLs[0]=https://192.168.100.200:6443")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "helm template failed: %s", out)

		dec := yaml.NewDecoder(strings.NewReader(string(out)))
		var spec map[string]any
		for {
			var doc map[string]any
			if derr := dec.Decode(&doc); derr != nil {
				break
			}
			if doc["kind"] == "DaemonSet" {
				spec = doc["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			}
		}
		require.NotNil(t, spec)

		agent := findByName(t, spec["containers"].([]any), "agent")
		env := agent["env"].([]any)
		hostEnv := findByName(t, env, "KUBERNETES_SERVICE_HOST")
		require.NotEmpty(t, hostEnv["value"], "must not render an empty KUBERNETES_SERVICE_HOST - InClusterConfig rejects that outright")
	})
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

// TestAgentDaemonSet_ImageDigestPin verifies agent.image.digest, when set,
// overrides agent.image.tag for every agent-image container (both
// initContainers and the main container) - a deploy pinned to a digest must
// not have any container silently fall back to the floating tag.
func TestAgentDaemonSet_ImageDigestPin(t *testing.T) {
	const digest = "sha256:f0802317aa9d6fe1b3a748d2895c3df5f090c0b37b83cb51678a393498d51123"

	t.Run("digest set overrides tag everywhere", func(t *testing.T) {
		spec := renderAgentDaemonSet(t, "--set", "agent.image.digest="+digest)

		want := "ghcr.io/malformed-c/constellation-agent@" + digest
		for _, ic := range spec["initContainers"].([]any) {
			m := ic.(map[string]any)
			require.Equal(t, want, m["image"], "initContainer %q must use the pinned digest", m["name"])
		}
		agent := findByName(t, spec["containers"].([]any), "agent")
		require.Equal(t, want, agent["image"])
	})

	t.Run("digest unset falls back to tag", func(t *testing.T) {
		spec := renderAgentDaemonSet(t)
		agent := findByName(t, spec["containers"].([]any), "agent")
		require.Equal(t, "ghcr.io/malformed-c/constellation-agent:main", agent["image"])
	})
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// TestAgentDaemonSet_ArchNodeSelector verifies the agent's nodeSelector
// excludes non-amd64 nodes by construction (kubernetes.io/arch: amd64),
// rather than via a maintained list of excluded node names - an arm64 node
// (no working Cilium/BPF datapath today) is excluded automatically, no
// chart update needed when a new one joins the cluster. Also checks the
// nodeAffinity/excludeNodeNames mechanism this replaced stays gone.
func TestAgentDaemonSet_ArchNodeSelector(t *testing.T) {
	spec := renderAgentDaemonSet(t)

	nodeSelector, _ := spec["nodeSelector"].(map[string]any)
	require.Equal(t, "amd64", nodeSelector["kubernetes.io/arch"])

	require.Nil(t, spec["affinity"], "node exclusion must be via nodeSelector, not a nodeAffinity NotIn list")
}

// TestAgentDaemonSet_LabelDomain pins the peri.apsis label domain that the
// chart depends on, for both keys and for two different reasons:
//
//   - nodeSelector peri.apsis/primary decides where the agent DaemonSet is
//     SCHEDULED. If this key stops matching what perigeos writes, the
//     DaemonSet schedules on zero nodes and every agent is evicted.
//   - managedPawnsSelector peri.apsis/host decides which pawn nodes each
//     agent MANAGES. If this stops matching, the agent silently falls back
//     to managing only its own node (pkg/k8s/tables/nodewatcher.go) and
//     IPAM drops to host-scope (pkg/ipam/ipam.go) - while the agent stays
//     Running and reports healthy. That failure does not announce itself,
//     which is exactly why it is worth a test.
//
// Renamed from periapsis.io/* on 2026-08-02 in lockstep with periapsis.
// Kubernetes label selectors have no OR across keys, so these must match
// the labels on the nodes exactly - there is no partial-credit state.
func TestAgentDaemonSet_LabelDomain(t *testing.T) {
	spec := renderAgentDaemonSet(t)

	nodeSelector, _ := spec["nodeSelector"].(map[string]any)
	require.Equal(t, "true", nodeSelector["peri.apsis/primary"],
		"DaemonSet scheduling selector must use the peri.apsis domain")
	require.NotContains(t, nodeSelector, "periapsis.io/primary",
		"the old periapsis.io domain must not linger alongside the new one")

	agent := findByName(t, spec["containers"].([]any), "agent")
	args := toStringSlice(agent["args"])
	require.Contains(t, args, "--managed-pawns-selector=peri.apsis/host")
	for _, a := range args {
		require.NotContains(t, a, "periapsis.io/",
			"no argument may still reference the old label domain")
	}
}

// TestAgentDaemonSet_K8sAPIServerURLTiers verifies the chart renders the TIERED
// form: a nested list becomes ONE --k8s-api-server-urls value with its members
// space-joined (the agent splits on whitespace to recover the tier), while a
// bare string stays a one-member tier. Getting this wrong is silent - the agent
// would simply treat every URL as its own tier and lose the load spreading.
func TestAgentDaemonSet_K8sAPIServerURLTiers(t *testing.T) {
	spec := renderAgentDaemonSet(t,
		"--set", "k8sAPIServerURLs[0]=https://127.0.0.1:6443",
		"--set", "k8sAPIServerURLs[1][0]=https://10.0.0.1:6443",
		"--set", "k8sAPIServerURLs[1][1]=https://10.0.0.2:6443",
		"--set", "k8sAPIServerURLs[2]=https://192.168.100.200:6443",
	)
	args := toStringSlice(findByName(t, spec["containers"].([]any), "agent")["args"])

	require.Contains(t, args, "--k8s-api-server-urls=https://127.0.0.1:6443")
	require.Contains(t, args, "--k8s-api-server-urls=https://10.0.0.1:6443 https://10.0.0.2:6443",
		"a nested list must render as ONE flag value, space-joined")
	require.Contains(t, args, "--k8s-api-server-urls=https://192.168.100.200:6443")

	// Tier order must survive templating.
	i0 := indexOf(args, "--k8s-api-server-urls=https://127.0.0.1:6443")
	i1 := indexOf(args, "--k8s-api-server-urls=https://10.0.0.1:6443 https://10.0.0.2:6443")
	i2 := indexOf(args, "--k8s-api-server-urls=https://192.168.100.200:6443")
	require.Less(t, i0, i1)
	require.Less(t, i1, i2)

	// The members must NOT have been emitted as separate tiers.
	require.NotContains(t, args, "--k8s-api-server-urls=https://10.0.0.1:6443",
		"tier members must not be split into one flag each - that loses the tier")
}

// TestAgentDaemonSet_RpFilterFixDoesNotApplyContainerSysctls guards the
// inversion that caused the 2026-08-03 silent pod-egress outage.
//
// The fix-sysctls init container exists to turn reverse-path filtering OFF on
// the host. It used to write the correct file to /host-etc/sysctl.d and then
// run `sysctl --system` — but the host's /etc is mounted at /host-etc, so that
// command reads THIS CONTAINER's sysctl.d, not the host's. The agent image
// ships /usr/lib/sysctl.d/55-network-security.conf with rp_filter=2, and the
// pod is hostNetwork + privileged, so it applied the container's hardening
// defaults to the host and set rp_filter=2 — the opposite of its purpose.
//
// The result was invisible: pods whose veth kept rp_filter=2 had every egress
// packet discarded by the kernel after the veth tap and before conntrack, so
// there was no cilium drop event and no unhealthy pod. Three workloads ran
// dead for five hours reporting Ready the whole time.
func TestAgentDaemonSet_RpFilterFixDoesNotApplyContainerSysctls(t *testing.T) {
	spec := renderAgentDaemonSet(t)

	init := findByName(t, spec["initContainers"].([]any), "fix-sysctls")

	// Assert on the EXECUTED lines only. The script comments necessarily
	// discuss `sysctl --system` in order to explain why it must not be used,
	// and a naive substring check matches that prose and fails on a correct
	// script — the same "grep matched the text about the thing, not the
	// thing" trap this file's other tests exist to avoid.
	var executed []string
	for line := range strings.SplitSeq(strings.Join(toStringSlice(init["command"]), "\n"), "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			executed = append(executed, line)
		}
	}
	script := strings.Join(executed, "\n")
	require.Contains(t, script, "rp_filter",
		"sanity: the stripped script must still contain the sysctl work, "+
			"otherwise these NotContains assertions would pass vacuously")

	require.NotContains(t, script, "sysctl --system",
		"must not run `sysctl --system`: it reads the CONTAINER's sysctl.d "+
			"(host /etc is at /host-etc) and would apply the image's "+
			"rp_filter=2 to the host via hostNetwork")

	// Setting the live values is what actually takes effect; writing the file
	// alone only helps at the next boot.
	require.Contains(t, script, "sysctl -w net.ipv4.conf.default.rp_filter=0")
	require.Contains(t, script, "sysctl -w net.ipv4.conf.all.rp_filter=0")

	// Existing veths keep the value they were born with and cilium only writes
	// rp_filter on devices it creates, so without this sweep every pod adopted
	// across a cold boot stays silently dead until something recreates it.
	require.Contains(t, script, "/proc/sys/net/ipv4/conf/lxc*/rp_filter",
		"must sweep pre-existing lxc devices, not just fix future ones")

	// The inversion survived because the container's failure was silenced.
	require.NotContains(t, script, "|| true",
		"must not swallow errors: that is why the inverted sysctl went unnoticed")
	require.NotContains(t, script, ">/dev/null 2>&1",
		"must not discard output: this container's failure needs to be visible")
}

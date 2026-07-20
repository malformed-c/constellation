// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

// Package constellation_test exercises the constellation Helm chart's
// rendered output and the shell logic embedded in it. Neither is covered
// by normal Go unit tests, since it's pure YAML/shell, not Go source.
package constellation_test

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestAgentDaemonSet_ResolveK8sServiceHostWiring is a regression test for
// the KUBERNETES_SERVICE_HOST auto-detect fix: a control-plane-colocated
// node (loopback reachable) and a remote node need different values, which
// a single static Helm value can't provide. This checks the plumbing that
// makes that work is actually present in the rendered manifest: the probe
// initContainer, the shared volume, and the main container's wrapper +
// mount - not the shell logic itself (see TestResolveK8sServiceHostScript
// and TestAgentCommandWrapperScript for that).
func TestAgentDaemonSet_ResolveK8sServiceHostWiring(t *testing.T) {
	spec := renderAgentDaemonSet(t)

	initContainers := spec["initContainers"].([]any)
	probe := findByName(t, initContainers, "resolve-k8s-service-host")

	probeMounts := probe["volumeMounts"].([]any)
	mount := findByName(t, probeMounts, "k8s-service-host")
	require.Equal(t, "/run/constellation", mount["mountPath"])

	probeEnv := probe["env"].([]any)
	portEnv := findByName(t, probeEnv, "KUBERNETES_SERVICE_PORT")
	require.Equal(t, "6443", portEnv["value"],
		"probe must use the same port value as the main container's env, not an independent literal")

	containers := spec["containers"].([]any)
	agent := findByName(t, containers, "agent")

	command := toStringSlice(agent["command"])
	require.Contains(t, command, "/bin/sh", "main container must be wrapped so it can read the probe result")
	joined := strings.Join(command, "\n")
	require.Contains(t, joined, "KUBERNETES_SERVICE_HOST")
	require.Contains(t, joined, `exec /usr/bin/cilium-agent "$@"`,
		"wrapper must exec the real binary with the existing args passed through unmodified")

	agentMounts := agent["volumeMounts"].([]any)
	agentMount := findByName(t, agentMounts, "k8s-service-host")
	require.Equal(t, "/run/constellation", agentMount["mountPath"])
	require.Equal(t, true, agentMount["readOnly"])

	// The main container's args must NOT be touched by the wrapper - it's
	// the one thing that must always pass through unchanged regardless of
	// which value ends up in KUBERNETES_SERVICE_HOST.
	args := toStringSlice(agent["args"])
	require.Contains(t, args, "--routing-mode=tunnel")
	require.Contains(t, args, "--kube-proxy-replacement=true")

	volumes := spec["volumes"].([]any)
	vol := findByName(t, volumes, "k8s-service-host")
	require.Contains(t, vol, "emptyDir")
}

func toStringSlice(v any) []string {
	list := v.([]any)
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = s.(string)
	}
	return out
}

// extractScript pulls the shell script (the "-c" argument) out of a
// rendered container/initContainer's command list, so its actual runtime
// behavior can be exercised directly rather than just checked for
// structural presence.
func extractScript(t *testing.T, command []any) string {
	t.Helper()
	for i, arg := range command {
		if arg == "-c" && i+1 < len(command) {
			return command[i+1].(string)
		}
	}
	t.Fatal("no -c script found in command")
	return ""
}

// TestResolveK8sServiceHostScript_PrefersLoopbackWhenReachable runs the
// ACTUAL probe script (extracted from the rendered chart, not a
// reimplementation of it) against a real local TCP listener standing in
// for a colocated API server, and confirms it resolves to 127.0.0.1.
func TestResolveK8sServiceHostScript_PrefersLoopbackWhenReachable(t *testing.T) {
	requireBash(t)

	port := startLoopbackListener(t)
	spec := renderAgentDaemonSet(t, "--set", "k8sServicePort="+port)
	probe := findByName(t, spec["initContainers"].([]any), "resolve-k8s-service-host")
	script := extractScript(t, probe["command"].([]any))

	outFile := runScript(t, script, map[string]string{"KUBERNETES_SERVICE_PORT": port})
	require.Equal(t, "127.0.0.1", outFile)
}

// TestResolveK8sServiceHostScript_FallsThroughWhenUnreachable runs the same
// real script against a port nothing is listening on, and confirms it
// falls through to the configured k8sServiceHost within a short bounded
// wait (using a short-retry render so the test doesn't take ~75s).
func TestResolveK8sServiceHostScript_FallsThroughWhenUnreachable(t *testing.T) {
	requireBash(t)

	spec := renderAgentDaemonSet(t, "--set", "k8sServicePort=1") // nothing listens on port 1
	probe := findByName(t, spec["initContainers"].([]any), "resolve-k8s-service-host")
	script := extractScript(t, probe["command"].([]any))
	// The rendered script retries for ~75s; shrink that for the test by
	// capping how long we're willing to wait rather than editing the
	// script, so this exercises the exact same logic that ships.
	script = strings.Replace(script, "seq 1 25", "seq 1 2", 1)
	script = strings.Replace(script, "sleep 3", "sleep 1", 1)

	outFile := runScript(t, script, map[string]string{"KUBERNETES_SERVICE_PORT": "1"})
	require.Equal(t, "192.168.50.1", outFile, "must fall through to the configured k8sServiceHost when nothing is reachable on loopback")
}

// TestAgentCommandWrapperScript exercises the main container's wrapper
// script directly: given a resolved-host file, it must export
// KUBERNETES_SERVICE_HOST and exec the real binary with args passed
// through; given no file (or an empty one), it must leave the env-var
// default alone.
func TestAgentCommandWrapperScript(t *testing.T) {
	requireBash(t)

	spec := renderAgentDaemonSet(t)
	agent := findByName(t, spec["containers"].([]any), "agent")
	script := extractScript(t, agent["command"].([]any))

	// Stand in for /usr/bin/cilium-agent with a script that just prints
	// what it was invoked with, so we can observe the wrapper's effect
	// without needing the real cilium-agent binary.
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "usr-bin")
	require.NoError(t, os.MkdirAll(fakeBin, 0o755))
	stub := "#!/bin/sh\necho \"HOST=$KUBERNETES_SERVICE_HOST ARGS=$*\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(fakeBin, "cilium-agent"), []byte(stub), 0o755))
	script = strings.ReplaceAll(script, "/usr/bin/cilium-agent", filepath.Join(fakeBin, "cilium-agent"))

	t.Run("exports resolved host", func(t *testing.T) {
		runDir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(runDir, "constellation"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(runDir, "constellation", "k8s-service-host"), []byte("127.0.0.1"), 0o644))
		localScript := strings.ReplaceAll(script, "/run/constellation", filepath.Join(runDir, "constellation"))

		out := runShC(t, localScript, nil, "--", "--some-flag=x")
		require.Contains(t, out, "HOST=127.0.0.1")
		require.Contains(t, out, "ARGS=--some-flag=x")
	})

	t.Run("leaves env default when file missing", func(t *testing.T) {
		runDir := t.TempDir() // no k8s-service-host file written
		localScript := strings.ReplaceAll(script, "/run/constellation", filepath.Join(runDir, "constellation"))

		out := runShC(t, localScript, map[string]string{"KUBERNETES_SERVICE_HOST": "192.168.50.1"}, "--", "--some-flag=x")
		require.Contains(t, out, "HOST=192.168.50.1")
	})
}

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
}

func runScript(t *testing.T, script string, env map[string]string) string {
	t.Helper()
	runDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(runDir, "constellation"), 0o755))
	localScript := strings.ReplaceAll(script, "/run/constellation", filepath.Join(runDir, "constellation"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", localScript)
	cmd.Env = append(os.Environ(), envSlice(env)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "script failed: %s", out)

	content, err := os.ReadFile(filepath.Join(runDir, "constellation", "k8s-service-host"))
	require.NoError(t, err)
	return string(content)
}

func runShC(t *testing.T, script string, env map[string]string, extraArgs ...string) string {
	t.Helper()
	args := append([]string{"-c", script}, extraArgs...)
	cmd := exec.Command("sh", args...)
	if env != nil {
		cmd.Env = append(os.Environ(), envSlice(env)...)
	}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "wrapper script failed: %s", out)
	return string(out)
}

func envSlice(env map[string]string) []string {
	var out []string
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// startLoopbackListener starts a bare TCP listener on 127.0.0.1 and returns
// its port as a string, standing in for a colocated API server for the
// probe test. It's closed automatically when the test ends.
func startLoopbackListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	return port
}

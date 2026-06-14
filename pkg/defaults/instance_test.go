// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Constellation

package defaults

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// resetInstanceDefaults captures every instance-scoped variable mutated by
// SetInstanceID and returns a cleanup function that restores them. Because
// these are package-level vars, every test that calls SetInstanceID must defer
// the returned function so it does not leak state into sibling tests.
func resetInstanceDefaults() func() {
	saved := struct {
		runtimePath, libraryPath, sockPath, shellSockPath   string
		monitorSock, pidFile, deleteQueueDir, deleteQueueLF string
		bpffsRoot, certsDir                                 string
		hostDev, secondHostDev, geneveDev, vxlanDev         string
		ipip4Dev, ipip6Dev                                  string
	}{
		RuntimePath, LibraryPath, SockPath, ShellSockPath,
		MonitorSockPath1_2, PidFilePath, DeleteQueueDir, DeleteQueueLockfile,
		BPFFSRoot, CertsDirectory,
		HostDevice, SecondHostDevice, GeneveDevice, VxlanDevice,
		IPIPv4Device, IPIPv6Device,
	}
	return func() {
		RuntimePath, LibraryPath, SockPath, ShellSockPath = saved.runtimePath, saved.libraryPath, saved.sockPath, saved.shellSockPath
		MonitorSockPath1_2, PidFilePath, DeleteQueueDir, DeleteQueueLockfile = saved.monitorSock, saved.pidFile, saved.deleteQueueDir, saved.deleteQueueLF
		BPFFSRoot, CertsDirectory = saved.bpffsRoot, saved.certsDir
		HostDevice, SecondHostDevice, GeneveDevice, VxlanDevice = saved.hostDev, saved.secondHostDev, saved.geneveDev, saved.vxlanDev
		IPIPv4Device, IPIPv6Device = saved.ipip4Dev, saved.ipip6Dev
	}
}

// TestSetInstanceID_NamespacesPathRoots verifies that the runtime/library/bpffs
// roots are rewritten under the instance id, since these are the roots from
// which every other path is derived.
func TestSetInstanceID_NamespacesPathRoots(t *testing.T) {
	defer resetInstanceDefaults()()

	SetInstanceID("pawn-7")

	require.Equal(t, "/var/run/cilium/pawn-7", RuntimePath)
	require.Equal(t, "/var/lib/cilium/pawn-7", LibraryPath)
	require.Equal(t, "/sys/fs/bpf/constellation/pawn-7", BPFFSRoot)
}

// TestSetInstanceID_DerivedPathsTrackRuntimePath verifies that every path
// derived from RuntimePath stays consistent with the rewritten root. This is
// the whole point of declaring these as vars rather than upstream's consts:
// the derived chain must follow the root after a rewrite.
func TestSetInstanceID_DerivedPathsTrackRuntimePath(t *testing.T) {
	defer resetInstanceDefaults()()

	SetInstanceID("pawn-7")

	require.Equal(t, RuntimePath+"/cilium.sock", SockPath)
	require.Equal(t, RuntimePath+"/shell.sock", ShellSockPath)
	require.Equal(t, RuntimePath+"/monitor1_2.sock", MonitorSockPath1_2)
	require.Equal(t, RuntimePath+"/cilium.pid", PidFilePath)
	require.Equal(t, RuntimePath+"/deleteQueue", DeleteQueueDir)
	require.Equal(t, DeleteQueueDir+"/lockfile", DeleteQueueLockfile)
	require.Equal(t, RuntimePath+"/certs", CertsDirectory)
}

// TestSetInstanceID_NamespacesInterfaceNames verifies that the host veth pair
// and tunnel devices are suffixed with the instance id so devices from
// different agents on the same host do not collide.
func TestSetInstanceID_NamespacesInterfaceNames(t *testing.T) {
	defer resetInstanceDefaults()()

	SetInstanceID("pawn-7")

	require.Equal(t, "cilium_host_pawn-7", HostDevice)
	require.Equal(t, "cilium_net_pawn-7", SecondHostDevice)
	require.Equal(t, "cilium_geneve_pawn-7", GeneveDevice)
	require.Equal(t, "cilium_vxlan_pawn-7", VxlanDevice)
	require.Equal(t, "cilium_ipip4_pawn-7", IPIPv4Device)
	require.Equal(t, "cilium_ipip6_pawn-7", IPIPv6Device)
}

// TestSetInstanceID_DistinctIDsDoNotCollide verifies the core multi-tenancy
// guarantee: two different instance ids produce fully disjoint path roots and
// interface names, so two agents can coexist on one host.
func TestSetInstanceID_DistinctIDsDoNotCollide(t *testing.T) {
	defer resetInstanceDefaults()()

	// Only values that are themselves namespaced under the id (the path roots
	// and interface names). Derived paths like SockPath end in "/cilium.sock"
	// and are covered by TestSetInstanceID_DerivedPathsTrackRuntimePath.
	snapshot := func() []string {
		return []string{
			RuntimePath, LibraryPath, BPFFSRoot,
			HostDevice, SecondHostDevice, GeneveDevice, VxlanDevice, IPIPv4Device, IPIPv6Device,
		}
	}

	SetInstanceID("longid-a")
	first := snapshot()

	SetInstanceID("longid-b")
	second := snapshot()

	for i := range first {
		require.NotEqual(t, first[i], second[i],
			"instance-scoped value %d must differ between ids 'a' and 'b'", i)
	}
	// Every value must end in its own id segment ("/<id>" for paths, "_<id>"
	// for interface names) and never carry the other instance's id.
	for _, v := range first {
		require.True(t, strings.HasSuffix(v, "/longid-a") || strings.HasSuffix(v, "_longid-a"),
			"value %q should be namespaced under id 'longid-a'", v)
		require.NotContains(t, v, "longid-b")
	}
	for _, v := range second {
		require.True(t, strings.HasSuffix(v, "/longid-b") || strings.HasSuffix(v, "_longid-b"),
			"value %q should be namespaced under id 'longid-b'", v)
		require.NotContains(t, v, "longid-a")
	}
}

// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Constellation

package defaults

// Constellation fork addition (instance-id). This file isolates the entire
// instance-scoped path/interface surface in ONE place so the upstream files
// (defaults.go, node.go) only differ from Cilium by the *absence* of these
// declarations — a clean deletion that rarely conflicts on rebase, rather
// than an in-place const→var rewrite.
//
// These identifiers are declared here (instead of as consts in defaults.go /
// node.go) precisely because SetInstanceID must rewrite them at startup so
// multiple Cilium agents can coexist on one host — the prerequisite for
// perigeos host-sharding / multi-tenancy.

import "fmt"

// Instance-scoped path roots. They start at Cilium's conventional defaults and
// are rewritten by SetInstanceID for multi-instance deployments. Anything that
// upstream declared relative to RuntimePath lives here so the whole derived
// chain stays consistent after a rewrite.
var (
	// RuntimePath is the default path to the runtime directory.
	RuntimePath = "/var/run/cilium"

	// LibraryPath is the default path to the cilium libraries directory.
	LibraryPath = "/var/lib/cilium"

	// SockPath is the path to the UNIX domain socket exposing the API to clients locally.
	SockPath = RuntimePath + "/cilium.sock"

	// ShellSockPath is the path to the UNIX domain socket exposing the debug shell
	// to which "cilium-dbg shell" connects to.
	ShellSockPath = RuntimePath + "/shell.sock"

	// MonitorSockPath1_2 is the path to the UNIX domain socket used to
	// distribute BPF and agent events to listeners (1.2 protocol version).
	MonitorSockPath1_2 = RuntimePath + "/monitor1_2.sock"

	// PidFilePath is the path to the pid file for the agent.
	PidFilePath = RuntimePath + "/cilium.pid"

	// DeleteQueueDir is the directory used for the CNI plugin to queue deletion
	// requests if the agent is down.
	DeleteQueueDir = RuntimePath + "/deleteQueue"

	// DeleteQueueLockfile is the file used to synchronise access of the queue
	// directory between the agent and the CNI plugin processes.
	DeleteQueueLockfile = DeleteQueueDir + "/lockfile"

	// BPFFSRoot is the default path where BPFFS should be mounted. When an
	// instance-id is set this becomes a per-instance sub-mount so that BPF
	// maps from different instances do not collide.
	BPFFSRoot = "/sys/fs/bpf"

	// CertsDirectory is the default directory used to find certificates
	// specified in the L7 policies.
	CertsDirectory = RuntimePath + "/certs"
)

// Interface names created by the agent. Scoped so the host veth pair and
// tunnel devices from different instances do not conflict on the same host.
var (
	// HostDevice is the name of the device that connects the cilium IP
	// space with the host's networking model.
	HostDevice = "cilium_host"

	// SecondHostDevice is the name of the second interface of the host veth pair.
	SecondHostDevice = "cilium_net"

	// IPIPv4Device is a device of type 'ipip', created by the agent.
	IPIPv4Device = "cilium_ipip4"

	// IPIPv6Device is a device of type 'ip6tnl', created by the agent.
	IPIPv6Device = "cilium_ipip6"

	// GeneveDevice is a device of type 'geneve', created by the agent.
	GeneveDevice = "cilium_geneve"

	// VxlanDevice is a device of type 'vxlan', created by the agent.
	VxlanDevice = "cilium_vxlan"
)

// SetInstanceID namespaces all instance-scoped path roots and interface names
// under id so that multiple Cilium agents can coexist on the same host.
// It must be called before any of the variables above are consumed — i.e.
// before flag defaults are evaluated in NewAgentCmd (see preScanInstanceID).
func SetInstanceID(id string) {
	RuntimePath = fmt.Sprintf("/var/run/cilium/%s", id)
	LibraryPath = fmt.Sprintf("/var/lib/cilium/%s", id)
	BPFFSRoot = fmt.Sprintf("/sys/fs/bpf/constellation/%s", id)

	SockPath = RuntimePath + "/cilium.sock"
	ShellSockPath = RuntimePath + "/shell.sock"
	MonitorSockPath1_2 = RuntimePath + "/monitor1_2.sock"
	PidFilePath = RuntimePath + "/cilium.pid"
	DeleteQueueDir = RuntimePath + "/deleteQueue"
	DeleteQueueLockfile = DeleteQueueDir + "/lockfile"
	CertsDirectory = RuntimePath + "/certs"

	HostDevice = "cilium_host_" + id
	SecondHostDevice = "cilium_net_" + id
	GeneveDevice = "cilium_geneve_" + id
	VxlanDevice = "cilium_vxlan_" + id
	IPIPv4Device = "cilium_ipip4_" + id
	IPIPv6Device = "cilium_ipip6_" + id
}

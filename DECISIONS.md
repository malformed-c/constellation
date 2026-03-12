# Architecture Decisions

## ADR-001: Multi-pawn agent topology

**Date:** 2026-03-12  
**Status:** Decided — implementing Option B (`--managed-nodes`)

### Context

Perigeos enables host sharding: multiple virtual k8s nodes (pawns) run on a single physical host. Each pawn appears as a distinct `Node` object in the k8s API with its own `spec.nodeName`. Constellation (the Cilium fork) must be able to provide CNI networking for pods scheduled across all pawns on a host.

The core tension is that Cilium was designed with a strict one-agent-per-host assumption. Its pod watcher filters on `spec.nodeName=<thisHost>`, its endpoint restore rejects pods from other node names, and its runtime paths and BPF maps are singletons scoped to the host.

### Options considered

---

#### Option A: `--instance-id` (one agent per pawn)

Run one constellation-agent per pawn, with all singleton resources namespaced under the pawn name.

**Changes:**
- `pkg/defaults/defaults.go` — RuntimePath, LibraryPath, BPFFSRoot become vars, rewritten by `SetInstanceID(id)`
- `pkg/defaults/node.go` — interface names (cilium_host, cilium_net, etc.) become vars, suffixed with `_<id>`
- `daemon/cmd/root.go` — `preScanInstanceID()` pre-parses `--instance-id` before cobra
- Three additional files with hardcoded `/var/run/cilium` paths fixed

**Pros:**
- Strong isolation between pawns — separate BPF maps, separate policy enforcement
- Failure of one agent doesn't affect other pawns
- Closest to standard Cilium operational model

**Cons:**
- Resource overhead: N agents per host, N BPF map sets, N sets of interfaces
- Requires toolchain support for multi-agent lifecycle management
- Makes rebasing onto upstream Cilium harder (more files touched)
- k8s field selector `spec.nodeName=<pawn>` works correctly per-agent, but IPAM and CiliumNode objects need one per pawn

**Implementation status:** Partially implemented in current `main` (`daemon/cmd/`, `pkg/defaults/`). Can be revived if Option B proves insufficient.

---

#### Option B: `--managed-nodes` (one agent per host) ← **current approach**

Run one constellation-agent per physical host. The agent manages pods across all pawns by watching multiple node names simultaneously.

**Changes:**
- `daemon/k8s/pods.go` — replace single `spec.nodeName=X` field selector with multiple watches (one per managed node name), merged into the same `LocalPod` statedb table
- `daemon/cmd/endpoint_restore.go` — relax `pod.Spec.NodeName != nodeTypes.GetName()` check to accept any name in the managed-nodes set
- `daemon/cmd/daemon_main.go` — register `--managed-nodes` flag (comma-separated list, defaults to hostname)
- `pkg/node/types/nodename.go` — expose `GetManagedNames() []string` alongside `GetName()`

**Pros:**
- Single BPF map namespace — lower overhead, simpler operations
- Single agent lifecycle — easier to manage, easier to reason about
- Smaller diff from upstream Cilium
- Natural fit: one host, one network datapath owner

**Cons:**
- k8s API server load: N list/watch connections per host instead of one (mitigated by label/field selector approach)
- Agent failure affects all pawns on the host simultaneously
- IPAM still needs to account for multiple virtual nodes sharing one agent's address space

**Implementation:** In progress.

---

#### Option C: Base-name + counter pattern

Pawns are named `<hostname>-0`, `<hostname>-1`, etc. The agent watches a prefix rather than explicit names.

**Changes:**
- Perigeos config enforces the naming convention
- Agent derives managed node names from `<hostname>-*` pattern at startup
- No k8s API changes needed for node registration

**Pros:**
- Zero agent changes for the pod watcher — derive names algorithmically
- Predictable naming makes automation easy

**Cons:**
- Naming convention is a hard constraint on the operator — no freeform pawn names
- Prefix-based k8s field selectors don't exist; still need multiple watches or in-process filtering
- Couples physical hostname to pawn naming in a way that's hard to change later

**Implementation status:** Not started. Available as fallback if Option B has multi-watch reliability issues.

---

### Decision

**Option B** (`--managed-nodes`) is the right starting point. One agent per host is architecturally simpler, has lower resource overhead, and the required changes are contained to two files in the agent. The `--managed-nodes` flag defaults to the hostname, making it a zero-config change for standard Cilium deployments.

Option A code is preserved in the repository and can be reinstated if strong pawn isolation (separate BPF maps, independent failure domains) becomes a requirement. Option C is available as a fallback if multi-watch proves unreliable at scale.

# ADR-0001: Per-pawn CIDR allocation for managed-nodes IPAM

**Status:** Accepted  
**Date:** 2026-03-20  
**Repository:** constellation

## Context

Constellation's `--managed-nodes-selector` feature lets a single agent manage pods across multiple virtual nodes (pawns) on the same host. The agent watches endpoints, policies, and CiliumNodes for all managed pawns. However, IPAM allocation was not updated to match — it still operates as if one agent manages one node with one CIDR.

### Current behavior

The constellation-operator allocates a per-node CIDR (e.g., /20 from the `10.0.0.0/8` pool) by writing it into each pawn's CiliumNode `.spec.ipam.podCIDRs`. The agent initializes a `hostScopeAllocator` using only its own node identity (the infrastructure pawn, determined by `K8S_NODE_NAME`). When a CNI ADD arrives for any pod — regardless of which pawn scheduled it — the agent allocates from this single CIDR.

### What breaks

- Pods on `pawn-worker-03` get IPs from `pawn-infra-01`'s CIDR
- The Kubernetes Node object's `podCIDR` for `pawn-worker-03` doesn't match its actual pod IPs
- Network policies reasoning about source node CIDRs are wrong
- `cilium_ipcache` entries map pod IPs to the wrong tunnel endpoint when multi-host is involved
- Route tables on remote hosts point the pawn's CIDR at the wrong tunnel destination
- Any observability that assumes pod-IP → node mapping (Hubble, Prometheus node labels) is incorrect

### Why this wasn't caught earlier

Single-host development. With one host and one Constellation agent, all pods share the same tunnel endpoint regardless of pawn. The wrong-CIDR allocation is invisible until a second host tries to route to a pod IP via the pawn's advertised CIDR.

## Decision

Extend the Constellation agent's IPAM to allocate from per-pawn CIDRs when running in managed-nodes mode. Two changes are required: one in perigeos (CNI client), one in constellation (agent IPAM).

### Change 1: Pass node name in CNI args (perigeos)

`ConstellationNetworkManager.runtimeConf()` in `internal/network/constellation.go` currently passes:

```go
Args: [][2]string{
    {"K8S_POD_NAMESPACE", namespace},
    {"K8S_POD_NAME", name},
    {"K8S_POD_UID", podUID},
},
```

Add the pod's scheduled node name:

```go
Args: [][2]string{
    {"K8S_POD_NAMESPACE", namespace},
    {"K8S_POD_NAME", name},
    {"K8S_POD_UID", podUID},
    {"K8S_POD_NODE_NAME", nodeName},
},
```

`nodeName` is `pod.Spec.NodeName`, available in Gambit at CNI setup time. Thread it through the `NetworkManager.Setup` signature.

### Change 2: Multi-pool IPAM in the agent (constellation)

Replace the single `hostScopeAllocator` with a node-keyed allocator map when managed-nodes mode is active.

#### IPAM initialization

On startup, the agent:

1. Reads its own CiliumNode (infrastructure pawn) — this is the existing behavior
2. Lists all CiliumNodes matching the managed-nodes selector
3. For each CiliumNode with `.spec.ipam.podCIDRs`, creates a `hostScopeAllocator` scoped to that CIDR
4. Stores allocators in a `map[string]*hostScopeAllocator` keyed by node name

The agent already watches managed CiliumNodes for endpoint state. Extend this watch to also trigger allocator creation/removal when CiliumNodes are added or deleted (pawn scale-up/down).

#### Allocation path

`allocateIPsWithCiliumAgent` in `plugins/cilium-cni/cmd/cmd.go` calls `client.IPAMAllocate()`. The agent-side handler needs to:

1. Read `K8S_POD_NODE_NAME` from the CNI args (passed through the IPAM request)
2. Look up the allocator for that node name
3. Allocate from the node-specific pool
4. Fall back to the infrastructure pawn's pool if the node name is unknown (defensive)

#### Deallocation path

`IPAMReleaseIP` needs to identify which pool an IP belongs to and release it to the correct allocator. Since CIDRs don't overlap (the operator allocates disjoint ranges), this is a prefix match against the allocator map.

#### Data model

The IPAM allocation result already includes the pool name. Use the node name as the pool identifier so the CNI plugin can verify the allocation came from the expected pool.

### Wire format

The CNI args flow through the existing path:

```
perigeos (CNI client)
  → cilium-cni plugin (reads K8S_POD_NODE_NAME from CNI_ARGS)
    → agent REST API (IPAMAllocate with owner="namespace/pod", pool="pawn-worker-03")
      → per-node hostScopeAllocator
```

No new API endpoints are needed. The existing `POST /ipam` endpoint gains awareness of the pool parameter that is already in its signature but unused in cluster-pool mode.

## Consequences

- Pod IPs correctly fall within their scheduled pawn's advertised CIDR
- Route tables on remote hosts correctly map pawn CIDRs to tunnel endpoints
- Network policies and Hubble observe correct node→pod IP mappings
- The `NetworkManager.Setup` signature changes to include node name — all callers in Gambit must be updated
- IPAM state is more complex: N allocators instead of 1. Memory overhead is negligible (one bitmap per /20). Locking must be per-allocator or a single lock with the map lookup inside
- If a CiliumNode is deleted while pods are still running on that pawn, the allocator must not be removed until all IPs are released. Reference-count or defer removal to the next sweep
- Single-pawn deployments (no managed-nodes) are unaffected — the allocator map has one entry

# AGENTS.md — Guide for AI agents working on Constellation

Constellation is a minimal fork of Cilium adapted for the Perigeos host-sharding model. Read this before making changes.

---

## What this repo is

One Cilium agent per physical host manages pods across all virtual nodes (pawns) using `--managed-nodes-selector` for label-based discovery. The diff versus upstream Cilium is intentionally tiny.

Every change should be evaluated: *does this need to be a fork change, or can it go upstream?*

---

## Repository structure

Standard Cilium layout. Constellation-specific changes:

### Multi-node management (active)

| File | Purpose |
|------|---------|
| `daemon/cmd/daemon_main.go` | `--managed-nodes-selector` flag registration |
| `pkg/option/config.go` | `ManagedNodesSelector` field, bare-key auto-expansion |
| `daemon/k8s/pods.go` | `NewPodTableAndReflector` creates per-node reflectors; integrates node watcher |
| `daemon/k8s/nodes.go` | node watcher: `discoverManagedNodes` (List) + `startNodeWatcher` (Watch) |
| `pkg/node/types/nodename.go` | `SetManagedNames`/`GetManagedNames`/`IsManaged` API |
| `daemon/cmd/endpoint_restore.go` | relaxed node name check for managed names |
| `pkg/ipam/managed_scope.go` | `managedScopeAllocator` — merges per-pawn CIDRs into one round-robin IPAM pool |

### Instance scoping (preserved, not active)

`pkg/defaults/defaults.go`, `pkg/defaults/node.go`, `pkg/bpf/bpffs_linux.go`, `daemon/cmd/root.go` — instance-scoped paths via `--instance-id`. Currently unused; kept for reference.

---

## Key flag: --managed-nodes-selector

Pass a bare label key (e.g. `perigeos.io/host`) — it auto-expands to `perigeos.io/host=<os.Hostname()>`. The agent then:

1. **Startup**: Lists nodes matching the selector, calls `SetManagedNames`, creates per-node pod reflectors
2. **Runtime**: Watches for node add/remove, dynamically registers new reflectors
3. **Fallback**: If no selector or no matching nodes, behaves like stock Cilium (single local node)

Tests in `daemon/k8s/managed_nodes_test.go` cover both paths.

## IPAM: managedScopeAllocator

Each pawn CiliumNode has its own pod CIDR (e.g. `/20`). `managedScopeAllocator` merges all pawn CIDRs into one pool and allocates round-robin. This allows one agent to manage 30+ pawns × 4094 IPs each.

The allocator is initialized with the primary node's CIDR plus CIDRs fetched from all managed CiliumNodes at startup.

---

## Key deployment flags

```
--managed-nodes-selector=perigeos.io/host
--routing-mode=tunnel
--kube-proxy-replacement=true
--ipam=cluster-pool
--bpf-lb-sock-hostns-only=true   ← critical: restricts socket LB to host cgroup only
```

`--bpf-lb-sock-hostns-only=true` is required. Without it, socket LB runs in pod cgroups and rewrites service VIPs at `connect()` time, bypassing packet-level NAT. The return path (`cil_to_netdev`) then has no CT state and sends replies to the LAN instead of back into the pod.

---

## Ground rules

**Keep the diff small.** The entire constellation delta should rebase onto new Cilium releases without significant conflict. If a change touches more than ~10 files, question whether it belongs here.

**eBPF-only.** Requires `--kube-proxy-replacement=true`. Do not add iptables/nftables support.

**Never hardcode singleton paths.** Use `defaults.RuntimePath`, `defaults.LibraryPath`, `defaults.BPFFSRoot`, and interface name vars.

**`ManagedNodesSelector` not `ManagedNodeSelector`.** The flag was renamed to clarify it manages multiple nodes. Keep this consistent — the old name caused confusion.

---

## Making changes

1. **Multi-node management** — extending node/pod discovery. Keep in `daemon/k8s/` and `pkg/node/types/`.
2. **Instance-scoping** — a singleton becoming instance-aware. Follow `pkg/defaults/` pattern.
3. **Constellation-specific feature** — something Perigeos needs that Cilium won't want. Keep isolated.
4. **Upstream fix** — contribute to Cilium directly, then rebase in.

---

## Rebasing onto upstream Cilium

```bash
git remote add upstream https://github.com/cilium/cilium.git
git fetch upstream

BASE=$(git log --oneline | tail -1 | awk '{print $1}')
NEW_TREE=$(git rev-parse <upstream-commit>^{tree})
NEW_BASE=$(git commit-tree $NEW_TREE -m "Cilium <version> (squashed upstream base)")
git rebase --onto $NEW_BASE $BASE main
```

Conflicts will most likely occur in `pkg/defaults/` and `pkg/option/config.go`.

---

## CI

- `lint-go.yaml` — golangci-lint + go mod tidy + unit tests
- `lint-bpf.yaml` — BPF datapath checks
- `build-images.yaml` — builds and pushes `constellation-agent`, `constellation-operator` to `ghcr.io/malformed-c/` on merge to main

## Versioning

Tracks Cilium: `v1.20.x-constellation.N`. Tag format: `v1.20.0-constellation.1`.

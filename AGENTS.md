# AGENTS.md — Guide for AI agents working on Constellation

Constellation is a minimal fork of Cilium adapted for the Perigeos host-sharding model. Read this before making changes.

---

## What this repo is

One Cilium agent per physical host manages pods across all virtual nodes (pawns) using `--managed-pawns-selector` for label-based discovery. The diff versus upstream Cilium is intentionally tiny.

Every change should be evaluated: *does this need to be a fork change, or can it go upstream?*

---

## Repository structure

Standard Cilium layout. Constellation-specific changes:

### Multi-node management (active)

| File | Purpose |
|------|---------|
| `daemon/cmd/daemon_main.go` | `--managed-pawns-selector` flag registration |
| `pkg/option/config.go` | `ManagedPawnsSelector` field, bare-key auto-expansion |
| `daemon/k8s/pods.go` | `NewPodTableAndReflector` creates per-node reflectors; integrates node watcher |
| `daemon/k8s/nodes.go` | node watcher: `discoverManagedNodes` (List) + `startNodeWatcher` (Watch) |
| `pkg/node/types/nodename.go` | `SetManagedNames`/`GetManagedNames`/`IsManaged` API |
| `daemon/cmd/endpoint_restore.go` | relaxed node name check for managed names |
| `pkg/ipam/managed_scope.go` | `managedScopeAllocator` — merges per-pawn CIDRs into one round-robin IPAM pool |

### Scale-to-zero (STZ) datapath (active)

| File | Purpose |
|------|---------|
| `bpf/lib/constellation_stz.h` | SYN-trap/wake hook (`constellation_stz_ingress4`), called from `local_delivery.h`. Gated by `CONSTELLATION_ENABLE_STZ` / `--enable-scale-to-zero-datapath`. |
| `pkg/maps/constellationstz` | Go access to the pinned `constellation_stz_triggers`/`constellation_stz_flows` maps. `Clear(ip)` deletes both maps' entries for an IP. |
| `pkg/ipam/allocator.go` (`clearScaleToZeroState`) | Calls `constellationstz.Clear` on both IP allocate and release, so a trigger leaked by a periapsis crash/restart can never outlive the IP it was armed for (bare-IP-keyed maps have no pod UID/generation binding — see git history for the incident that motivated this). |

Arming/disarming policy (idle detection, wake handling) lives in periapsis's `internal/activator/`, not here — this repo only owns the datapath maps, the drop/wake hook, and the IPAM-side leak guard.

### Pod-endpoint watchdog (active)

| File | Purpose |
|------|---------|
| `pkg/podendpointwatchdog/watchdog.go` | Periodically cross-checks Running pods (with a `PodIP`) across all managed pawns against `endpointmanager`, and deletes any pod that's continuously observed missing its local Cilium endpoint for at least the grace period. |

Heals a confirmed production failure mode: an endpoint lost across an agent restart (while the pod's netns/IP linger) is invisible to both IPAM and the agent, and nothing else re-triggers CNI ADD for it — the pod just stays `Running` with a dead datapath (`EHOSTUNREACH` from the host). Deleting the pod forces Kubernetes/perigeos to recreate it with a fresh CNI ADD. Gated by `--enable-pod-endpoint-watchdog` (default `true`); interval/grace period via `--pod-endpoint-watchdog-interval` (default `60s`) / `--pod-endpoint-watchdog-grace-period` (default `90s`). Does **not** catch an endpoint that exists but is policy-dropped (a separate, unconfirmed failure mode) — only missing-endpoint.

**Grace period is self-timed, not pod-age-based.** It deliberately does *not* read `pod.Status.StartTime` — a live incident found a control plane that never populates that field (always `nil`), which silently disabled an earlier StartTime-based grace check on every pod, forever. The grace period is instead measured from the first time the watchdog's own scan observed the pod missing its endpoint (`watchdog.pending`), so it works regardless of what the control plane does or doesn't populate on pod status.

### Instance scoping (preserved, not active)

`pkg/defaults/defaults.go`, `pkg/defaults/node.go`, `pkg/bpf/bpffs_linux.go`, `daemon/cmd/root.go` — instance-scoped paths via `--instance-id`. Currently unused; kept for reference.

---

## Key flag: --managed-pawns-selector

Pass a bare label key (e.g. `periapsis.io/host`) — it auto-expands to `periapsis.io/host=<os.Hostname()>`. The agent then:

1. **Startup**: Lists nodes matching the selector, calls `SetManagedNames`, creates per-node pod reflectors
2. **Runtime**: Watches for node add/remove, dynamically registers new reflectors
3. **Fallback**: If no selector or no matching nodes, behaves like stock Cilium (single local node)

Tests in `pkg/k8s/tables/managed_nodes_test.go` cover both paths.

## KUBERNETES_SERVICE_HOST auto-detection

`k8sServiceHost` (the API server address, required — kube-proxy is replaced so the agent has no kube-proxy-provided path to it) is a single Helm value shared by every node running the DaemonSet. But whether the API server happens to be reachable on a given node's own loopback is a per-node runtime fact (true for a control-plane-colocated node, false for a remote one) — a single static value can't be correct for both.

`agent-daemonset.yaml`'s `resolve-k8s-service-host` initContainer handles this without any per-node config: since the pod is `hostNetwork: true`, it polls `127.0.0.1:<k8sServicePort>` (bounded retry, ~75s, to survive a cold boot where the local API server and this initContainer start concurrently) and writes `127.0.0.1` to a shared `emptyDir` if something answers there, else falls through to the configured `k8sServiceHost` unchanged. The main container's `command` is a small `sh -c` wrapper that reads that file and `export`s it over the env-var default before exec'ing `cilium-agent` — an initContainer can't mutate another container's env directly, so this file-then-export handoff is required.

## IPAM: managedScopeAllocator

Each pawn CiliumNode has its own pod CIDR (e.g. `/20`). `managedScopeAllocator` merges all pawn CIDRs into one pool and allocates round-robin. This allows one agent to manage 30+ pawns × 4094 IPs each.

The allocator is initialized with the primary node's CIDR plus CIDRs fetched from all managed CiliumNodes at startup.

---

## Key deployment flags

```
--managed-pawns-selector=periapsis.io/host
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

**`ManagedPawnsSelector` not `ManagedNodesSelector`/`ManagedNodeSelector`.** The flag was renamed twice: first to clarify it manages multiple nodes, then again to match periapsis's "pawn" terminology for the Node objects it manages. Keep this consistent — old names cause confusion.

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

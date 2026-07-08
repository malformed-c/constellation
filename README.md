# Constellation

Constellation is a fork of [Cilium](https://github.com/cilium/cilium) adapted for the [Perigeos](https://github.com/malformed-c/perigeos) host-sharding model, where multiple virtual Kubernetes nodes (pawns) run on a single physical host managed by a single CNI agent.

## Why fork?

Cilium assumes one agent per physical host. Its BPF maps, network interfaces, and runtime paths are singletons — `cilium_host`, `/var/run/cilium/cilium.sock`, `/sys/fs/bpf/tc/globals/`, etc.

Constellation adds `--managed-nodes-selector`, a label selector that lets one agent manage all pawn nodes sharing a host. The agent handles IPAM, endpoint management, and datapath for all pawns simultaneously.

## What changed from Cilium

| Feature | Description |
|---|---|
| `--managed-nodes-selector` | Label selector for discovering pawn nodes. Pass a bare label key (e.g. `periapsis.io/host`) to auto-append `=<hostname>`. |
| `managedScopeAllocator` | IPAM allocator that merges per-pawn CIDRs into a single round-robin pool. Each pawn gets its own `/20` (or configured size) from its CiliumNode. |
| Pod reflector | Watches pods across all managed node names, not just the local node. |
| Endpoint restore | Restores endpoints for pods on any managed node after agent restart. |
| Scale-to-zero (STZ) datapath | SYN-trap/wake mechanism for pods idled by periapsis. Gated by `--enable-scale-to-zero-datapath`. See [Scale-to-zero datapath](#scale-to-zero-stz-datapath) below. |

Everything else derives from Cilium unmodified.

## Deployment

Constellation runs as a DaemonSet (or perigeos-managed pod) on the physical host node:

```yaml
args:
  - --managed-nodes-selector=periapsis.io/host
  - --routing-mode=tunnel
  - --kube-proxy-replacement=true
  - --ipam=cluster-pool
  - --bpf-lb-sock-hostns-only=true
```

The `--managed-nodes-selector=periapsis.io/host` flag auto-appends `=<hostname>`, so the agent discovers all nodes labeled `periapsis.io/host=<this-host>`.

See `deploy/constellation/` in the perigeos repo for full manifests.

## IPAM

Each pawn node has a CiliumNode with its own pod CIDR. Constellation's `managedScopeAllocator` merges all pawn CIDRs into one pool and allocates round-robin. This scales to 30+ pawns × 4094 IPs per pawn from a single agent.

The constellation-operator (part of perigeos) creates and manages the CiliumNode resources.

## Scale-to-zero (STZ) datapath

`bpf/lib/constellation_stz.h` implements a SYN-trap for pods that periapsis has idled ("scaled to zero"). Three pinned BPF maps back it:

| Map | Key | Value | Purpose |
|---|---|---|---|
| `constellation_stz_triggers` | pod IPv4 | armed flag | Idled pods. A bare SYN to an armed IP is dropped and a wake event is emitted instead of being delivered. |
| `constellation_stz_flows` | pod IPv4 | last-seen timestamp | Every managed pod IP, updated on each ingress packet — periapsis uses this to decide when a pod has gone idle. |
| `constellation_stz_events` | — | ringbuf | Wake events (dest pod IP) consumed by periapsis to un-idle the pod. |

Arming/disarming and the idle-detection policy live entirely in periapsis (`internal/activator/`), not in this repo — Constellation only owns the datapath maps and the drop/wake hook. Only pods opted in via the `periapsis.io/scale-to-zero` annotation are ever armed.

**IPAM cleanup:** the maps are keyed by bare IP with no pod UID/generation binding, so a trigger left behind by a periapsis crash/restart (before it could disarm a deleted pod) would otherwise survive independently of any pod — and silently black-hole every future SYN to that IP if Kubernetes later reassigns it to an unrelated pod. `pkg/maps/constellationstz` + hooks in `pkg/ipam/allocator.go` (`clearScaleToZeroState`, called on both allocate and release) purge any stale trigger/flow entry for an IP the moment IPAM hands it out or frees it, so a leaked entry can never outlive the IP it was armed for.

## Images

```
ghcr.io/malformed-c/constellation-agent     — CNI agent
ghcr.io/malformed-c/constellation-operator  — CiliumNode/IPAM operator
```

## Relationship to Cilium

Constellation tracks Cilium's `main` branch. The diff is intentionally minimal to keep rebasing straightforward. Upstream Cilium history is squashed into a single base commit; Constellation-specific commits follow on top.

## License

Apache 2.0, same as Cilium. See [LICENSE](LICENSE).

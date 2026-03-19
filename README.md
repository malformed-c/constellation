# Constellation

Constellation is a fork of [Cilium](https://github.com/cilium/cilium) adapted for the [Perigeos](https://github.com/malformed-c/perigeos) host-sharding model, where multiple virtual Kubernetes nodes (pawns) run on a single physical host managed by a single CNI agent.

## Why fork?

Cilium assumes one agent per physical host. Its BPF maps, network interfaces, and runtime paths are singletons — `cilium_host`, `/var/run/cilium/cilium.sock`, `/sys/fs/bpf/tc/globals/`, etc.

Constellation adds `--managed-nodes-selector`, a label selector that lets one agent manage all pawn nodes sharing a host. The agent handles IPAM, endpoint management, and datapath for all pawns simultaneously.

## What changed from Cilium

| Feature | Description |
|---|---|
| `--managed-nodes-selector` | Label selector for discovering pawn nodes. Pass a bare label key (e.g. `perigeos.io/host`) to auto-append `=<hostname>`. |
| `managedScopeAllocator` | IPAM allocator that merges per-pawn CIDRs into a single round-robin pool. Each pawn gets its own `/20` (or configured size) from its CiliumNode. |
| Pod reflector | Watches pods across all managed node names, not just the local node. |
| Endpoint restore | Restores endpoints for pods on any managed node after agent restart. |

Everything else derives from Cilium unmodified.

## Deployment

Constellation runs as a DaemonSet (or perigeos-managed pod) on the physical host node:

```yaml
args:
  - --managed-nodes-selector=perigeos.io/host
  - --routing-mode=tunnel
  - --kube-proxy-replacement=true
  - --ipam=cluster-pool
  - --bpf-lb-sock-hostns-only=true
```

The `--managed-nodes-selector=perigeos.io/host` flag auto-appends `=<hostname>`, so the agent discovers all nodes labeled `perigeos.io/host=<this-host>`.

See `deploy/constellation/` in the perigeos repo for full manifests.

## IPAM

Each pawn node has a CiliumNode with its own pod CIDR. Constellation's `managedScopeAllocator` merges all pawn CIDRs into one pool and allocates round-robin. This scales to 30+ pawns × 4094 IPs per pawn from a single agent.

The constellation-operator (part of perigeos) creates and manages the CiliumNode resources.

## Images

```
ghcr.io/malformed-c/constellation-agent     — CNI agent
ghcr.io/malformed-c/constellation-operator  — CiliumNode/IPAM operator
```

## Relationship to Cilium

Constellation tracks Cilium's `main` branch. The diff is intentionally minimal to keep rebasing straightforward. Upstream Cilium history is squashed into a single base commit; Constellation-specific commits follow on top.

## License

Apache 2.0, same as Cilium. See [LICENSE](LICENSE).

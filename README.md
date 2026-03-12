# Constellation

Constellation is a fork of [Cilium](https://github.com/cilium/cilium) designed for the [Perigeos](https://github.com/malformed-c/perigeos) host sharding model, where multiple virtual nodes (pawns) run on a single physical host.

## Why fork?

Cilium assumes one agent per host. Its BPF maps, network interfaces, sockets, and runtime paths are all named as singletons — `cilium_host`, `/var/run/cilium/cilium.sock`, `/sys/fs/bpf/tc/globals/`, etc. Running two Cilium agents on the same host causes them to fight over these resources.

Constellation adds a single `--instance-id` flag that namespaces every singleton under a unique identifier, allowing one agent per pawn rather than one agent per host.

## What changed from Cilium

Everything derives from four root paths that are now instance-scoped:

| Resource | Cilium | Constellation |
|---|---|---|
| Runtime dir | `/var/run/cilium/` | `/var/run/cilium/<id>/` |
| State dir | `/var/lib/cilium/` | `/var/lib/cilium/<id>/` |
| BPF pin path | `/sys/fs/bpf/` | `/sys/fs/bpf/constellation/<id>/` |
| Host interface | `cilium_host` | `cilium_host_<id>` |

Sockets, pidfile, certs, BPF map pins, and tunnel interfaces all inherit from these roots — no other callers needed changing.

**eBPF-only mode is required.** Constellation does not support iptables/nftables fallback paths. Run with `--kube-proxy-replacement=strict`.

## Usage

```bash
constellation-agent --instance-id=pawn-0 [other cilium flags...]
```

The `instance-id` must match the pawn name configured in Perigeos. Perigeos generates the CNI config automatically and passes the correct instance ID through the CNI invocation.

## Images

Images are published to `ghcr.io/malformed-c/`:

- `constellation-agent` — the main agent (fork of `cilium/cilium`)
- `constellation-operator` — cluster operator
- `constellation-hubble-relay` — Hubble relay

## Relationship to Cilium

Constellation tracks Cilium's `main` branch. The diff is intentionally minimal — one flag, four root path vars, and interface name vars. This keeps rebasing straightforward.

Upstream Cilium history is squashed into a single base commit. Constellation-specific commits follow on top.

## License

Apache 2.0, same as Cilium. See [LICENSE](LICENSE).

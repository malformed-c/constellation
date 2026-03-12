# Agent Instructions

This repo is Constellation, a minimal fork of Cilium. Read this before making changes.

## What this repo is

Constellation adds `--instance-id` to Cilium so multiple agents can coexist on one host. The diff versus upstream is intentionally tiny. Every change should be evaluated against the question: *does this need to be a fork change, or can it go upstream?*

## Repository structure

Standard Cilium layout. The constellation-specific changes live in:

- `pkg/defaults/defaults.go` — instance-scoped runtime/library/BPF paths
- `pkg/defaults/node.go` — instance-scoped interface names
- `pkg/bpf/bpffs_linux.go` — exported `SetBPFFSRoot`
- `daemon/cmd/root.go` — `preScanInstanceID()` pre-parses `--instance-id` before cobra
- `daemon/cmd/daemon_main.go` — flag registration
- `pkg/option/config.go` — `InstanceID` field on `DaemonConfig`

## Ground rules

**Keep the diff small.** The entire constellation delta should be rebased onto new Cilium releases without significant conflict. If a change touches more than ~10 files, question whether it belongs here.

**eBPF-only.** Constellation requires `--kube-proxy-replacement=strict`. Do not add support for iptables/nftables code paths — those are already removed from the CI workflows and will not be tested.

**Never hardcode singleton paths.** Any new code that creates files, sockets, or BPF pins must use `defaults.RuntimePath`, `defaults.LibraryPath`, `defaults.BPFFSRoot`, or the interface name vars — never raw strings like `/var/run/cilium` or `cilium_host`.

**Interface names are vars, not constants.** `defaults.HostDevice`, `defaults.SecondHostDevice`, etc. are now `var`. Treat them as runtime values.

## Making changes

Before touching anything, understand whether the change is:

1. **Instance-scoping** — a Cilium singleton that needs to become instance-aware. Follow the existing pattern in `pkg/defaults/`.
2. **Constellation-specific feature** — something Perigeos needs that Cilium will never want. Keep it isolated.
3. **Upstream fix** — should be contributed to Cilium directly, then rebased in.

## CI

Three workflows run on PR and push to main:

- `lint-go.yaml` — golangci-lint + go mod tidy check + unit tests
- `lint-bpf.yaml` — BPF datapath checks
- `build-images.yaml` — builds and pushes `constellation-agent`, `constellation-operator`, `constellation-hubble-relay` to `ghcr.io/malformed-c/` on merge to main and on version tags

The build targets `release` stage, not `debug`. Don't change this without understanding the arm64 cross-compilation implications.

## Rebasing onto upstream Cilium

```bash
git remote add upstream https://github.com/cilium/cilium.git
git fetch upstream

# Find the current squashed base commit (should be the first commit in log)
BASE=$(git log --oneline | tail -1 | awk '{print $1}')

# Create a new squashed base from the upstream target
NEW_TREE=$(git rev-parse <upstream-commit>^{tree})
NEW_BASE=$(git commit-tree $NEW_TREE -m "Cilium <version> (squashed upstream base)")

# Rebase constellation commits onto it
git rebase --onto $NEW_BASE $BASE main
```

Conflicts will most likely occur in `pkg/defaults/defaults.go` and `pkg/defaults/node.go` since those are the core changes. Resolve by re-applying the `SetInstanceID` pattern to whatever the upstream version looks like.

## Versioning

Constellation versions track Cilium: `v1.20.x-constellation.N`. Tag format: `v1.20.0-constellation.1`.

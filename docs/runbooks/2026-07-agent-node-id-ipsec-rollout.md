# Rollout: node-ID/IPsec + IngressAddressing fixes to `constellation-agent`

Coordinated-window rollout procedure for two datapath-adjacent fixes, banked
on `main` and CI-green but not yet deployed:

- `f1c1b0c290` — `nodeDelete`/`deleteIPsec` no longer tear down the shared
  BPF node-ID/IPsec-endpoint mapping when a *managed pawn's* CiliumNode is
  deleted (only the primary node was previously exempted). Without this,
  deleting any non-primary pawn's CiliumNode — a routine operation
  (reschedule, pod-endpoint-watchdog heal, etc.) — released the shared
  node ID for reuse by an unrelated remote node while the physical host and
  its other pawns were still up.
- `b8e8353db3` — `updateManagedCiliumInternalIPs` now also fans out
  `IngressAddressing` to managed pawn CiliumNodes (it already handled
  CiliumInternalIP and HealthAddressing). Currently dormant on this cluster
  since Envoy/Ingress isn't enabled, but the same class of bug.

This is a plan, not a log — nothing in this document has been executed as
of authoring. The decision to open the deploy window is engi's call; this
runbook exists so execution doesn't require re-deriving digests or
procedure under time pressure.

## Images

- **Target** (both fixes, built from `b8e8353db3`, CI green):
  `ghcr.io/malformed-c/constellation-agent@sha256:a16bfe60c73c61162e1c92976a485d7f52e4a16ba37d8836c667409b22f695f6`
  (linux/amd64 manifest digest — not the multi-arch index digest).
- **Rollback** (last known-good pre-fix, built from `08a1ca33ff`; the one
  commit between it and `f1c1b0c290` only touched a test file, so it's
  functionally identical to what's running today):
  `ghcr.io/malformed-c/constellation-agent@sha256:67f82765d12f8dac1d7a0cd4bd64802d70047400a19f7b08d9cffa050da95d1a`

Re-confirm the target digest immediately before applying — `:main` is a
floating tag and may have moved again since this was written.

## Hosts

Exactly two physical hosts run `constellation-agent` today:

- **engix99** — 7 pawns (`compute-09`, `engix99`, `engix99-e2e-1`,
  `engix99-e2e-2`, `engix99-scale-1`, `engix99-trail-1`,
  `engix99-trail-2`). This is the demo-day host (the perigeos
  restart-under-load scenario runs here).
- **engifire** — 3 pawns (`engifire`, `engifire-pawn-01`,
  `engifire-scale-1`).

**Canary order: engifire first, engix99 second.** Validate on the
non-demo host; touch the demo-stage host last, and only after the canary
and live smoke check are both clean.

## Mechanism

The DaemonSet is `maxSurge=0`/`maxUnavailable=1` `RollingUpdate`, but that
doesn't let you choose which host goes first, and DaemonSets have no
rollout-pause. Use `OnDelete` + manual pod deletion instead, to get full
control over ordering:

```bash
# 1. Make the template change inert until a pod is manually deleted.
kubectl patch ds/constellation-agent -n kube-system --type merge \
  -p '{"spec":{"updateStrategy":{"type":"OnDelete"}}}'

# 2. Update the image. Nothing happens to running pods yet.
kubectl set image ds/constellation-agent -n kube-system \
  agent=ghcr.io/malformed-c/constellation-agent@sha256:a16bfe60c73c61162e1c92976a485d7f52e4a16ba37d8836c667409b22f695f6

# 3. Roll engifire first.
kubectl delete pod -n kube-system constellation-agent-htjb2

# 4. Run the post-update gates below. Only if clean, roll engix99.
kubectl delete pod -n kube-system constellation-agent-4q44r

# 5. Run the post-update gates again. Once both hosts are confirmed
#    stable, restore the normal update strategy for future deploys.
kubectl patch ds/constellation-agent -n kube-system --type merge \
  -p '{"spec":{"updateStrategy":{"type":"RollingUpdate","rollingUpdate":{"maxSurge":0,"maxUnavailable":1}}}}'
```

(Pod names above are current as of authoring — re-check with
`kubectl get pods -n kube-system -l app.kubernetes.io/name=constellation-agent -o wide`
before running, in case they've been recreated since.)

## Pre-flight gates

- Both current agent pods `1/1 Ready`, 0 restarts, stable for at least an
  hour.
- No other concurrent cluster changes in flight — do not overlap with a
  control-plane bounce or the demo-2 k6 restart-under-load test.
- Target digest re-confirmed via `gh api /user/packages/container/constellation-agent/versions`
  immediately before applying.

## Post-update gates (run after each host, before proceeding to the next)

- New pod `Running`, `1/1`, 0 restarts, within 2 minutes.
- `kubectl exec` into it, `cilium status` shows all controllers `ok`, no
  IPAM errors.
- Every pawn on that host still `Ready` / `NetworkUnavailable=False` — this
  should never flap, since existing endpoints persist across an agent
  restart via the endpoint-restore path.
- No unexpected pod restarts on that host's pawns in the following 5
  minutes.
- A BPF node-ID map dump (`cilium-dbg bpf nodeid list` or equivalent, run
  inside the new agent pod) shows exactly one node-ID entry for the shared
  `CiliumInternalIP` — not duplicated, not orphaned.

## Live smoke check

Proves the node-ID fix holds live, not just "pod came up." Run once, after
the canary host (engifire) is confirmed healthy, targeting a non-critical
pawn on that host (e.g. `engifire-scale-1`):

1. Record the shared node-ID/IP entry via the BPF map dump.
2. `kubectl delete ciliumnode engifire-scale-1` — the CiliumNode CR only,
   **not** the underlying k8s Node. The pawn keeps running throughout.
3. Immediately re-check the BPF map dump: the shared entry **must still be
   present**. Pre-fix, this is exactly where it would have vanished.
4. Watch the CiliumNode self-heal/recreate via Constellation's own
   node-discovery loop (`kubectl get ciliumnode engifire-scale-1 -w`).
5. Confirm zero pod restarts or connectivity blips on that pawn throughout.

## Rollback

Same `OnDelete` mechanism in reverse:

```bash
kubectl set image ds/constellation-agent -n kube-system \
  agent=ghcr.io/malformed-c/constellation-agent@sha256:67f82765d12f8dac1d7a0cd4bd64802d70047400a19f7b08d9cffa050da95d1a
kubectl delete pod -n kube-system <affected-pod>   # one host at a time
```

Run the same post-update gates after rolling back. Restore
`updateStrategy` to `RollingUpdate` afterward either way.

## Watch during the bounce

```bash
kubectl get pods -n kube-system -l app.kubernetes.io/name=constellation-agent -w
kubectl get nodes -w
journalctl -u 'perigeos-*-agent.service' -f   # on the target host
```

Plus a spot-check of pod-to-pod connectivity on that host's pawns.

## Coordination

Serialize against periapsis-fable's queue — do not overlap her CP/resize
deploy or the demo-2 k6 restart-under-load test. The decision to open the
window is engi's.

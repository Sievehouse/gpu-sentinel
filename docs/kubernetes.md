# Kubernetes and NVSentinel

`capledger watch kubernetes` turns what Kubernetes says about its nodes into signed SLA events. It does for Kubernetes clusters what the Slurm watcher does for Slurm. The kernel-log agent on each node opens incidents when the GPU driver reports a fault, and the watcher records when the provider's control plane takes a node out of service and when it brings it back.

It reads four kinds of node state:

| State | Default | Turned on with |
| --- | --- | --- |
| `Ready` condition not `True` (`False` or `Unknown`) | down | on by default; `--not-ready=false` turns it off |
| Any other condition with status `True`, such as the health checks of [NVIDIA NVSentinel](https://github.com/NVIDIA/NVSentinel) (`SysLogsXIDError`, `GpuMemWatch`, `DCGMError`, …) or node-problem-detector (`KernelDeadlock`, `ReadonlyFilesystem`, …) | ignored | `--down-condition NAME` or a pattern such as `'*'`; `--degraded-condition` for degraded |
| Taints | ignored | `--down-taint KEY` or a pattern such as `'nvidia.com/gpu-*'`; `--degraded-taint` |
| Fewer allocatable GPUs (`nvidia.com/gpu` or `amd.com/gpu`) than expected | ignored | `--expected-gpus 8`; `--gpus-degrade` makes it degraded instead of down |
| Cordoned (`spec.unschedulable`) | ignored | `--cordoned` |

Rules to know:

- **Conditions.** NVSentinel and node-problem-detector set a condition to `True` while a fault lasts and back to `False` when it clears, which is what the watcher reads. Patterns use Go's `path.Match` syntax. They never match the kubelet's own conditions (`Ready`, `MemoryPressure`, `DiskPressure`, `PIDPressure`, `NetworkUnavailable`), which must be named exactly. An exact name beats a pattern: `--down-condition '*' --degraded-condition GpuThermalWatch` makes thermal trouble degraded and every other fault down.
- **Cordons are off by default.** Operators cordon nodes for many reasons. A health monitor that cordons on a fault, as NVSentinel does, also sets a condition, and the condition is what the watcher counts.
- **Causes.** Each event's cause names what Kubernetes said, such as `k8s:NotReady:KubeletNotReady`, `k8s:SysLogsXIDError:79` (the first bracketed error code in an NVSentinel message), `k8s:taint:nvidia.com/gpu-xid-error` or `k8s:gpus:7/8`. Several causes are joined with `+`. The full condition messages go in the event's note.
- **Times.** A down event is dated by the transition time of the condition that caused it, and a recovery by the latest change of a condition the rules look at. Times are clamped between the node's previous event and the moment of the view, and anything without a time (taints, GPU counts) is dated by the view. A watcher that was down for an hour therefore still records when Kubernetes says the node failed, not when the watcher noticed.
- **Missing nodes.** With `--contract`, the watcher records only the contract's nodes, and a contract node the cluster no longer lists is down (`k8s:node-missing`). Without it, a node that disappears after the watcher has seen it is down.
- **Node names.** If the cluster's node names differ from the contract's, label each node with its contract name and pass `--name-label example.com/contract-node`.
- **Restarts.** On start, the watcher reads its own log to learn which nodes it last recorded as down or degraded, so a restart neither repeats a down event nor forgets an open outage.
- **Failed reads.** If the API server cannot be reached, nothing is recorded. A failed read is not evidence that the nodes are down.

## Running it

In the cluster, with a read-only service account, as a single replica with its own volume (`deploy/kubernetes/node-watcher.yaml`):

```sh
kubectl create namespace capledger
kubectl apply -f deploy/kubernetes/node-watcher.yaml
```

From an operator's machine, with kubectl:

```sh
capledger watch kubernetes --from kubectl --context prod-gpu \
  --down-condition '*' --expected-gpus 8 --contract contract.yaml \
  --log k8s-events.jsonl --key-dir watcher-keys
```

Against an API server directly: `--api-server https://… --token-file … --ca-file …`. Without `--from`, the watcher uses the pod's service account inside a pod and kubectl elsewhere.

The watcher speaks for every node, like the Slurm watcher, so enrol its key without node bindings:

```sh
capledger keyring import --keyring keyring.json --log k8s-events.jsonl \
  --owner "Example Neocloud" --role provider-integration --bind-nodes=false
```

Contracts with a `sources` block admit its events through the `provider-integration` role (or the source name `kubernetes-node-watcher`).

## Trying it without a cluster

`examples/kubernetes/` holds two saved views of a three-node cluster, in `kubectl get nodes -o json` format. At 04:15 NVSentinel reports XID 79 on gpu-003 (which is also NotReady and cordoned), and gpu-004 has 7 of 8 GPUs. At 06:45 both are healthy again. `--at` says when a saved view was taken:

```sh
capledger watch kubernetes --from examples/kubernetes/nodes-2026-10-03T0415.json --at 2026-10-03T04:15:00Z --once \
  --down-condition '*' --expected-gpus 8 --log k8s.jsonl --key-dir k8s-keys
capledger watch kubernetes --from examples/kubernetes/nodes-2026-10-03T0645.json --at 2026-10-03T06:45:00Z --once \
  --down-condition '*' --expected-gpus 8 --log k8s.jsonl --key-dir k8s-keys
```

```
2026-10-03T04:12:00Z gpu-003 down k8s:NotReady:GpuHealthCheckFailed+k8s:SysLogsXIDError:79
2026-10-03T04:15:00Z gpu-004 down k8s:gpus:7/8
2026-10-03T06:40:00Z gpu-003 up k8s:healthy
2026-10-03T06:45:00Z gpu-004 up k8s:healthy
```

The outage on gpu-003 starts at 04:12:00, when NVSentinel's XID condition turned `True`, and ends at 06:40, when it turned `False`. Neither time is when the views were taken.

## Not the watcher's job

The watcher records what the provider's control plane says. It does not remediate, drain or cordon (NVSentinel and the provider's tooling do), and it is not an independent check: the [tenant probe](tenant-probe.md) is the customer's own record, and the contract's [reconcile rules](sla-spec.md) decide whose record counts.

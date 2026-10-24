# AMD Instinct GPUs

Version 0.2 adds AMD support to the agent, the tenant probe and the acceptance harness. It follows the public amdgpu driver and ROCm documentation but **has not yet been validated on AMD hardware in this repository**. Treat the starter policy as a draft to review with your AMD contacts before it drives contractual downtime.

## Agent

Run the agent with `--vendor amd`. It reads the same kernel log as on NVIDIA hosts and recognises these amdgpu messages:

| Class | Example message | Starter policy |
| --- | --- | --- |
| `ras_uncorrectable` | `amdgpu 0000:05:00.0: amdgpu: 1 uncorrectable hardware errors detected in umc block` | node_down |
| `ras_correctable` | `amdgpu 0000:05:00.0: amdgpu: 3 correctable hardware errors detected in umc block` | observe |
| `gpu_reset` | `amdgpu 0000:65:00.0: amdgpu: GPU reset begin!` | review |
| `gpu_reset_succeeded` | `amdgpu 0000:65:00.0: amdgpu: GPU reset(2) succeeded!` | observe |
| `gpu_reset_failed` | `amdgpu 0000:05:00.0: amdgpu: GPU reset(3) failed` | node_down |
| `ring_timeout` | `amdgpu 0000:65:00.0: [drm:amdgpu_job_timedout [amdgpu]] *ERROR* ring gfx_0.0.0 timeout, signaled seq=18342, emitted seq=18344` | review |
| `page_fault` | `amdgpu 0000:05:00.0: amdgpu: [gfxhub] page fault (src_id:0 ring:24 vmid:3 pasid:32769, …)` | app |

`observe` records the observation and nothing else; `review` and `app` work as for NVIDIA XIDs. Every recognised line is kept as a signed observation with the raw text, and node-level faults become `down` events with causes such as `amdgpu:gpu_reset_failed` or `amdgpu:ras_uncorrectable:umc`.

The agent also reads the driver's RAS error counters every `--ras-interval` (default one minute) from `/sys/class/drm/card*/device/ras/<block>_err_count`, whose lines read `ue: <count>` and `ce: <count>` ([kernel documentation](https://docs.kernel.org/6.9/gpu/amdgpu/ras.html)). The first reading is the baseline. Increases become observations classified like the kernel messages above (uncorrectable: node_down; correctable: observe), so an uncorrectable error is recorded even if its kernel message was lost. A counter that goes down, for example after a driver reload, is re-baselined rather than reported.

With `--expected-gpus`, the agent counts the PCI functions bound to the driver under `/sys/bus/pci/drivers/amdgpu` and records a `down` event when fewer are present. The agent reads sysfs directly rather than calling `amd-smi`, so its container needs no ROCm user space; the acceptance harness records `amd-smi` output instead. `--sys-root` points at the host's sysfs when it is mounted elsewhere; the DaemonSet example mounts it read-only at `/host/sys`.

The `amd` section of the policy file maps classes to actions, as the `xid` section does for NVIDIA ([`examples/policies/xid-default.json`](../examples/policies/xid-default.json)). Check how a log is classified with `capledger xid parse --file kern.log`, or replay it with `capledger agent --vendor amd --once --source kern.log …` ([`examples/samples/kern-amd.log`](../examples/samples/kern-amd.log) is a synthetic sample).

## Tenant probe

`capledger probe --vendor amd --expected-gpus 8` counts the amdgpu PCI functions visible to the tenant. The benchmark script runs on ROCm builds of PyTorch as well.

## Acceptance harness

Set `vendor: amd` in the inventory ([`examples/inventory/mi300x-2-node.yaml`](../examples/inventory/mi300x-2-node.yaml)):

| Stage | NVIDIA | AMD |
| --- | --- | --- |
| audit | `nvidia-smi` rows, topology, kernel, RDMA | amdgpu PCI functions from sysfs, then `amd-smi list`, `amd-smi static`, topology, kernel, RDMA |
| single_node | `dcgmi diag -r 3` | ROCm Validation Suite GPU stress test, `rvs -c /opt/rocm/share/rocm-validation-suite/conf/gst_single.conf` |
| collectives | nccl-tests | rccl-tests (same binaries and output format; point `nccl_tests_dir` at the build) |

The RVS stage rejects a node when `rvs` exits non-zero or prints a `pass: FALSE` or `met: FALSE` result line. RVS output differs between releases, so the raw output is kept for review. AMD's [system validation guide](https://instinct.docs.amd.com/projects/system-acceptance/en/latest/common/system-validation.html) recommends GPU-specific configurations (for example `conf/MI300X/gst_single.conf`) and AMD's field health check where it is installed; set `single_node_command` to use them. That command must exit non-zero when the node fails.

## Partitioned GPUs

MI300-series GPUs can run in compute and memory partition modes that expose more devices than there are physical GPUs. Check what a healthy node shows in the mode you contract for, and set `--expected-gpus` and `gpus_per_node` to that number.

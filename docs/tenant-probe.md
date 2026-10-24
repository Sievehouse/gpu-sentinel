# Tenant probe and corroboration

The provider's agent watches the hosts. The customer sees something else: whether its own jobs can use the GPUs it pays for. `capledger probe` gives the customer its own signed record, written from inside its allocation, and `capledger sla corroborate` compares the two records for the same contract and month.

## What the probe does

Every `--interval` (default 60 s) the probe:

1. Counts the GPUs it can see: on NVIDIA, the rows of `nvidia-smi --query-gpu=index,pci.bus_id,temperature.gpu,power.draw,clocks.sm --format=csv,noheader,nounits` that start with a GPU index (an "Unable to determine the device handle" line is not a GPU); on AMD (`--vendor amd`), the PCI functions bound to the `amdgpu` driver.
2. Optionally runs a benchmark every `--bench-every` (default 10 minutes) and compares the last number it prints with `--bench-floor`. [`harness/scripts/gemm_bench.py`](../harness/scripts/gemm_bench.py) prints the slowest GPU's TFLOPS, so one throttled GPU pulls the result below the floor. Calibrate the floor on healthy nodes of the same model.
3. Writes a signed heartbeat that includes the GPU count.

Every failed check is recorded as an observation. Events need a run of results, so a single blip does not open an incident, and each event carries the time of the first check of its run:

| Condition | Event | Cause |
| --- | --- | --- |
| `--fail-after` (default 3) failed checks in a row | `down` | `tenant:gpu-missing:7/8` or `tenant:probe-error` |
| then `--recover-after` (default 2) good checks in a row | `up` | `tenant:gpus-visible` |
| `--fail-after` benchmark results below the floor | `degraded_start` | `tenant:bench-below-floor:590<600` |
| then `--recover-after` results at or above it | `degraded_end` | `tenant:bench-recovered` |

A benchmark that fails to run or prints no number is recorded but counts neither way, because the fault may be the tool's. The probe only reads: it runs `nvidia-smi` and the benchmark command it was given, nothing else.

A check that has started is finished before the probe exits, so stopping the probe is never recorded as a failure. It does leave a gap in the heartbeats, which `capledger evidence gaps` shows.

## Running it

The probe must see the same GPUs as the workload.

- **Kubernetes:** the device plugin gives GPUs to one container, so start the probe in the background inside the workload's container: [`deploy/kubernetes/tenant-probe-job.yaml`](../deploy/kubernetes/tenant-probe-job.yaml).
- **Slurm:** run it as an overlapping job step that asks for the node's GPUs: [`deploy/slurm/tenant-probe.sbatch`](../deploy/slurm/tenant-probe.sbatch). Check once that `srun --overlap --gpus-per-node=8 nvidia-smi -L` inside a job lists every GPU on your cluster.

```bash
capledger probe --node gpu-003 --expected-gpus 8 --interval 60s \
  --log /evidence/probe-gpu-003.jsonl --key-dir /evidence/keys \
  --bench-cmd "python3 harness/scripts/gemm_bench.py --seconds 20" --bench-floor 600
```

The log and key belong to the tenant. Keep them on storage the tenant controls, copy them off the cluster regularly, and let the counterparty enrol the key from the log (`capledger keyring import --role tenant-probe`, see [keys and keyrings](keys-and-keyring.md)).

## Corroboration

```bash
capledger events export --keyring tenant-keyring.json \
  --log probe-gpu-001.jsonl --log probe-gpu-002.jsonl > tenant-probe.jsonl   # one --log per node
capledger sla corroborate --contract contract.yaml --period 2026-10 \
  --events-a provider.jsonl \
  --events-b maintenance-calendar.jsonl --events-b tenant-probe.jsonl
```

Both sides need the provider's maintenance calendar, because exclusions depend on it. Each side's events are run through the same contract (exclusions, minimum incident length, rack rule), then compared node by node: downtime that both recorded, downtime only one side recorded, and the exact stretches of each disagreement. Agreement is both / (both + only A + only B). Use `--format json` or `--out` for the full report, which is sealed with its own SHA-256 like a statement.

## Worked example: October 2026, `examples/contracts/hgx-16-node.yaml`

Side A is the provider's record, `examples/events/hgx-16-node-2026-10.jsonl`. Side B is the tenant's probe plus the maintenance calendar, `examples/events/hgx-16-node-2026-10-tenant.jsonl`.

```text
Contract   example-neocloud--example-lab--2026 (version 1), 2026-10-01T00:00:00Z to 2026-11-01T00:00:00Z
provider   availability 99.7021%, downtime 127680 node-seconds, credit 5%
tenant     availability 99.6823%, downtime 136160 node-seconds, credit 5%
Agreement  93.63% of all recorded downtime is seen by both; 5 node(s) in dispute

Node              provider     tenant       both only provider  only tenant
gpu-003               8880       8880       8820           60           60
gpu-005               3600       3720       3600            0          120
gpu-009                  0       3000          0            0         3000
gpu-011               7200       7200       7200            0            0
gpu-012                  0       5400          0            0         5400
gpu-014              14400      14400      14400            0            0
gpu-016              93600      93560      93560           40            0
```

| Node | What happened | Why the records differ |
| --- | --- | --- |
| gpu-003 | XID 79 at 04:12, repaired at 06:40. The probe's first failed check was 04:13 and its first good one 06:41. | One-minute sampling: 60 s only on the provider side at the start, 60 s only on the tenant side at the end. Expected. |
| gpu-005 | Down since before the period. The provider closed the incident at 01:00; the probe saw all GPUs again at 01:02. | 120 s only on the tenant side: the GPUs came back two minutes after the provider's health check said so. |
| gpu-009 | The probe saw 6 of 8 GPUs from 07:00 to 07:50. The provider recorded nothing. | 3,000 s only on the tenant side. This is what a second vantage point is for: a fault that left no XID, or a device problem only the tenant's containers saw. |
| gpu-010 | Announced maintenance 02:00–05:00; the probe failed from 02:00:30 to 05:00:30. | Excluded on both sides; the 30 s after the window is below the 5-minute minimum. Not listed. |
| gpu-011 | Unannounced maintenance 01:00–03:00. | Agreed: 7,200 s. |
| gpu-012 | Provider: `customer:oom-reboot`, which the contract excludes. Probe: 0 of 8 GPUs visible from 08:00 to 09:30. | 5,400 s only on the tenant side. The parties disagree about the cause; corroboration turns the argument into one 90-minute window to examine. |
| gpu-014 | Degraded 12:00–16:00, on both sides. | Agreed: 14,400 s. |
| gpu-016 | Rail 3 down at 22:00; the probe's first error was at 22:00:40. | 40 s only on the provider side. |

Totals: both 8,820 + 3,600 + 7,200 + 14,400 + 93,560 = 127,580 node-seconds; only the provider 60 + 40 = 100; only the tenant 60 + 120 + 3,000 + 5,400 = 8,580. Agreement = 127,580 / 136,260 = 93.63%. The tenant's own statement would show 136,160 node-seconds of downtime, 1 − 136,160 / 42,854,400 = 99.6823%. Both records give a 5% credit, so here the disagreements do not change the money, but gpu-009 and gpu-012 deserve an incident review. `internal/sla/corroborate_test.go` pins these numbers.

## Limits

- The probe sees what the tenant's containers see. A GPU hidden by the container runtime counts as missing: real unavailability for the tenant, but not necessarily a hardware fault.
- Outages shorter than `--fail-after` × `--interval` are not recorded at all. Keep that product well below the contract's minimum incident length (3 × 60 s against 5 minutes in the examples).
- The provider controls the hosts, so the probe is not proof against a determined provider. Its value is independence: the provider cannot alter the tenant's signed log without detection, and blocking the probe leaves a visible gap in its heartbeats.

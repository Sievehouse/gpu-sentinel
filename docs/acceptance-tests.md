# Acceptance tests

`capledger-harness plan` turns an inventory into ordered stages. Cheap per-node checks run first so that a broken node is caught before it spoils a multi-hour burn-in.

| Stage | Tool | Catches | Typical verdict on failure |
| --- | --- | --- | --- |
| audit | `nvidia-smi`, `nvidia-smi topo -m`, `uname`, `ibstat` (AMD: amdgpu PCI functions, `amd-smi`) | Missing GPUs, driver mismatches, wrong topology | reject (missing GPUs) |
| single_node | `dcgmi diag -r 3` (AMD: ROCm Validation Suite GPU stress test) | GPU memory, PCIe, NVLink and compute faults on one node | reject |
| rdma | `ib_write_bw` per rail, neighbouring node pairs | Dead or slow rails, wrong RDMA configuration | review |
| collectives | nccl-tests (AMD: rccl-tests) `all_reduce_perf`, `alltoall_perf` at 2, 4, 8 … nodes | Fabric and configuration problems that only appear at scale; wrong results | review (slow) or reject (wrong results) |
| burn_in | `harness/scripts/burnin.py` | Thermal and electrical faults under combined GPU and fabric load; slow GPUs | review (via peer comparison) |
| storage | `fio` sequential read and write | Misconfigured or slow shared storage | review |

Run the kernel-log agent during every stage. XIDs that appear under load are often the most important result. AMD Instinct clusters are covered in [AMD GPUs](amd-gpus.md).

## Handing over the result

The report is only as good as the trust in it. Pack the report, the plan, the raw results and the inventory into one signed bundle, so that every later reader can check that nothing was changed, added or removed:

```bash
capledger bundle create --out acceptance-example-hgx-32gpu.tar.gz --type acceptance \
  --subject example-hgx-32gpu --key-dir keys/harness \
  --file report.json --file report.md --file results.jsonl --file plan.json \
  --file inventory.yaml=examples/inventory/hgx-4-node.yaml --log evidence.jsonl
capledger bundle verify --bundle acceptance-example-hgx-32gpu.tar.gz --keyring keyring.json
```

The format and its checks are in [evidence formats](evidence-format.md#evidence-bundles).

## Why GPUs and fabric are stressed together

Stressing compute and network separately can miss failures that only appear under combined thermal and electrical load, a point SemiAnalysis makes strongly in its [ClusterMAX 3.0 methodology](https://newsletter.semianalysis.com/p/clustermax-30-the-industry-standard). The burn-in script overlaps large GEMMs on every GPU with all-to-all traffic across all ranks.

## Slow-but-passing nodes

Functional tests answer "does it work?". A node can pass every one and still be 10% slow, which drags down every synchronous job it joins. `capledger-harness outliers` compares each node with the median of its peers and flags it only when it is both statistically unusual (robust z-score beyond 3.5) and practically slower (5% or more by default).

## Localising a slow collective

When a 64-node all-reduce misses its floor, the harness does not test all 2,016 node pairs. It splits the failing group in half, tests both halves, and recurses; a single suspect is tested together with a node already known to be healthy. In simulation, two degraded nodes among 64 are found in about 23 tests over 7 rounds (`capledger-harness localize-sim`). On a real fabric, check that concurrent tests on disjoint groups do not interfere; if they do, run each round's tests one after another.

## Floors and criteria

`examples/criteria/hgx-h100-400g.yaml` holds illustrative floors. Calibrate floors from healthy clusters of the same GPU, NIC and topology before writing them into a contract, and keep them per world size: bus bandwidth depends on how many nodes take part.

## What acceptance cannot prove

- **Silent data corruption.** Synthetic microbenchmarks miss most SDC-defective GPUs, and many defects appear only with age ([OSDI '26](https://www.usenix.org/conference/osdi26/presentation/zheng)).
- **Slow-onset and intermittent faults.** Fail-slow behaviour can last minutes or hours and come and go ([USENIX ATC '25](https://www.usenix.org/conference/atc25/presentation/wu-tianyuan)).
- **Behaviour at a larger scale than tested.** A 32-GPU acceptance says little about a 2,048-GPU job's congestion.

That is why acceptance is followed by continuous evidence and why reports say "accept: no defect found by the tests that ran".

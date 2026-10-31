<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/banner-dark.svg">
  <img alt="GPU Sentinel: signed, replayable evidence of the GPU capacity a cluster actually delivered" src="docs/images/banner-light.svg" width="100%">
</picture>

<br>

[![ci](https://github.com/Sievehouse/gpu-sentinel/actions/workflows/ci.yml/badge.svg)](https://github.com/Sievehouse/gpu-sentinel/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/Sievehouse/gpu-sentinel?include_prereleases&label=release)](https://github.com/Sievehouse/gpu-sentinel/releases)
[![go](https://img.shields.io/github/go-mod/go-version/Sievehouse/gpu-sentinel?label=go)](go.mod)
[![python](https://img.shields.io/badge/python-3.10%2B-3776ab)](harness/pyproject.toml)
[![license](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![status](https://img.shields.io/badge/status-pre--alpha-orange)](#status)

**Contract-grade acceptance tests and SLA evidence for rented GPU clusters.**

[Quickstart](#quickstart) · [How it works](#how-it-works) · [Examples](#see-it-on-example-data) · [Docs](#documentation) · [Roadmap](#roadmap)

</div>

---

GPU Sentinel gives a GPU provider, its customer and their financiers **one neutral, replayable record of the capacity that was actually delivered**:

| | What you get | Who checks it |
| --- | --- | --- |
| **At handover** | A signed acceptance bundle: the test plan, raw results and per-node verdicts under one signature | The customer, before the first invoice |
| **Every month** | A sealed SLA statement that either party can recompute from the same signed events and get the same hash | Customer, insurer or lender, without trusting a dashboard |
| **When views differ** | A corroboration report comparing the provider's record with the customer's own probe, node by node, and contract rules that say whose clock counts | Both parties, before anyone argues about credits |
| **All month long** | A shared ledger both parties push their signed logs to, whose witness fixes everyone's history every day, and where each party signs the monthly statement | Both parties, and financiers once both consent |

<a id="status"></a>

> [!NOTE]
> **Pre-alpha (v0.3.0).** Everything here is implemented and tested on synthetic data. Nothing has been validated on production GPU clusters yet, and AMD support has not been run on AMD hardware. Example contracts are illustrative and are not legal advice.

## Why this exists

- **Delivered quality varies widely.** SemiAnalysis tested 77 GPU clouds for [ClusterMAX 3.0](https://newsletter.semianalysis.com/p/clustermax-30-the-industry-standard) (September 2026) and gave only 19 a medallion rating; its injected faults found health checks that missed failures or took hours to return nodes.
- **Contracts are standardising, measurement is not.** Providers and buyers now negotiate acceptance tests, downtime definitions and service credits, but each side measures with its own scripts and dashboards, so disputes are argued from support tickets.
- **Capital now needs the evidence.** Insurers sell parametric SLA cover and [lenders are starting to require it](https://capacityglobal.com/market-intelligence/data-centre-sla-risk-parametric-insurance-financing-2026/). A parametric trigger is only as good as the measurement behind it.
- **"Passed burn-in" is not "healthy".** ByteDance reports that synthetic microbenchmarks [miss over 60% of GPUs with silent data corruption](https://www.usenix.org/conference/osdi26/presentation/zheng), so acceptance has to be followed by continuous, comparable evidence.

## How it works

```mermaid
flowchart LR
  subgraph cluster["Inside the GPU cluster"]
    A["capledger agent<br/>XIDs, amdgpu, GPU count, heartbeats"] --> L[("signed evidence logs<br/>one key per node")]
    W["Slurm watcher<br/>drain and resume"] --> L
    P["capledger probe<br/>in the customer's jobs"] --> T[("customer's signed log")]
    H["acceptance harness<br/>nccl-tests, DCGM, RVS, fio"] --> B["signed acceptance bundle"]
  end
  L & T -->|"ledger push"| LG[("hosted ledger<br/>witness checkpoints")]
  LG -->|"pull, verify against keyring,<br/>export, derive gaps"| E{{"SLA engine<br/>contract as code"}}
  C["contract spec<br/>sources and reconcile rules"] --> E
  E --> S["sealed statement"]
  S -->|"sla sign by both parties"| LG
  S -->|replay| V["customer, insurer or lender<br/>recomputes the same hash"]
  LG -->|"feed, after both consent"| F["financier"]
  E -->|corroborate| R["agreement report"]
```

1. **Collect.** Agents, probes, integrations and the harness write signed entries to per-source evidence logs, each with its own key. Every entry carries the hash of the previous one, so edits, deletions and reordering are detectable, and heartbeats make silence visible.
2. **Share.** Both parties push their logs to a ledger for the contract. It accepts only entries that verify against its keyring, and its witness key signs a checkpoint of every log every day, so no history can be rewritten later.
3. **Compute.** The SLA engine replays availability events against the contract: minimum incident length, maintenance and customer-caused exclusions, a per-node exclusion cap, a rack rule for NVL72-style systems, unmonitored time, credit tiers, and whose clock counts when the provider and the customer recorded the same outage differently. All arithmetic is exact; there is no floating point in the money path.
4. **Agree.** The statement is sealed with a SHA-256 over its canonical JSON and fingerprints its inputs. Anyone with the contract and the signed events gets the same statement; each party signs it (accept or dispute), and financiers read it once both parties consent.

### A month in the life of a contract

```mermaid
sequenceDiagram
  autonumber
  participant N as GPU node (provider agent)
  participant Pr as Customer job (capledger probe)
  participant E as SLA engine
  participant C as Customer, insurer or lender
  loop every minute
    N->>N: heartbeat and fault checks, signed and chained
    Pr->>Pr: GPU and benchmark checks, in the customer's log
  end
  Note over N: XID 79 on gpu-003 becomes a signed "down" event
  N->>E: verified events, plus unmonitored gaps
  E->>C: sealed statement d6280aea… (99.7021%, 5% credit)
  C->>E: replay with the same contract and events
  E-->>C: same hash, so the statement stands
  Pr->>E: the customer's own events
  E-->>C: corroboration: 93.63% agreed, 5 nodes to review
  C->>E: both parties sign: accept (or dispute, with a note)
  Note over C: a lender reads the co-signed statement from the ledger feed
```

### Evidence that can't be quietly edited

```mermaid
flowchart LR
  K["#1 key<br/>gpu-003 announces<br/>key 655425e9…"] --> H1["#2 heartbeat<br/>prev = hash(#1)"]
  H1 --> O["#3 observation<br/>XID 79, raw kernel line<br/>prev = hash(#2)"]
  O --> D["#4 event: down<br/>cause xid:79<br/>prev = hash(#3)"]
  D --> U["#5 event: up<br/>prev = hash(#4)"]
  U --> M(["Merkle root<br/>over all entries"])
  KR[["keyring<br/>owner · role · nodes · validity"]] -.->|"may this key speak for gpu-003, at this time?"| D
```

Each entry is signed with Ed25519 by the node's own key. Change one byte, drop one line or swap two entries and `capledger evidence verify` fails. The keyring decides which keys are trusted, for which nodes and for how long ([keys and keyrings](docs/keys-and-keyring.md)).

## See it on example data

All charts below come from the repository's example contract and events, and you can reproduce every number with the commands under [Quickstart](#quickstart).

### One month, one sealed statement

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/sla-statement-dark.svg">
  <img alt="Terminal output of capledger sla compute: availability 99.7021%, credit 5% = 15000.00 USD, statement hash d6280aea" src="docs/images/sla-statement-light.svg" width="100%">
</picture>

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/sla-month-dark.svg">
  <img alt="Availability 99.7021% falls in the 99.5 to 99.9% tier: 5% of $300,000 = $15,000. Counted and excluded downtime per node." src="docs/images/sla-month-light.svg" width="100%">
</picture>

Four and a half hours of announced firmware work and a customer-caused reboot are excluded by the contract; the other 35.5 hours count. Every number is worked out by hand in [docs/sla-spec.md](docs/sla-spec.md), along with what happens when nodes stop sending heartbeats.

### Two vantage points, one comparison

The customer runs `capledger probe` inside its own jobs and keeps its own signed log. `capledger sla corroborate` lines the two records up:

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/corroboration-dark.svg">
  <img alt="93.63% of recorded downtime seen by both sides; 100 seconds only by the provider, 8,580 only by the tenant; five nodes in dispute" src="docs/images/corroboration-light.svg" width="100%">
</picture>

Instead of a month of support tickets, the parties have five windows to look at. [docs/tenant-probe.md](docs/tenant-probe.md) explains each one.

### Whose clock wins?

Both records of the same outage are defensible: the provider's agent saw the fault a minute before the customer's job did, and the job saw the GPUs back a minute after the provider's health check. Version 0.3 lets the contract settle that before anyone compares:

```yaml
measurement:
  reconcile:
    count: either     # an outage counts if either party recorded it (or: both, provider, tenant)
    start: provider   # it starts at the provider's first fault record (or: earliest, latest, tenant)
    end: tenant       # and ends when the tenant's jobs see the node again
sources:
  provider: { roles: [provider-agent, provider-integration] }
  tenant:   { roles: [tenant-probe] }
```

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/clock-rules-dark.svg">
  <img alt="gpu-003: the provider recorded 04:12 to 06:40, the tenant 04:13 to 06:41. Counting only what both saw gives 8,820 seconds; from the provider's first fault to the tenant's recovery gives 8,940 seconds." src="docs/images/clock-rules-light.svg" width="100%">
</picture>

Sources are matched by the keyring role of the key that signed each event, so neither party can pass its events off as the other's. Every rule is worked through by hand in the [SLA spec](docs/sla-spec.md#sources-and-reconcile-rules-which-clock-wins).

### Finding bad nodes fast

When a 64-node all-reduce misses its floor, the harness finds the culprits by splitting the group and recursing, instead of testing every pair of nodes (simulated with `capledger-harness localize-sim`):

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/fault-localisation-dark.svg">
  <img alt="Tests needed to find 2 bad nodes: group testing grows from 15 to 35 tests between 16 and 512 nodes; all pairs grows from 120 to 130,816" src="docs/images/fault-localisation-light.svg" width="100%">
</picture>

<details>
<summary>The numbers behind the charts</summary>

**SLA statement, October 2026** (`examples/contracts/hgx-16-node.yaml`)

| Node | Counted downtime | Excluded | Cause |
| --- | ---: | ---: | --- |
| gpu-016 | 93,600 s | | network: rail-3 link down |
| gpu-014 | 14,400 s | | NCCL bus bandwidth below floor |
| gpu-003 | 8,880 s | | XID 79 |
| gpu-011 | 7,200 s | | emergency patch, unannounced maintenance |
| gpu-005 | 3,600 s | | XID 48 |
| gpu-010 | | 10,800 s | firmware update announced 4 days ahead |
| gpu-012 | | 5,400 s | customer-caused (OOM reboot) |

**Corroboration** (seconds)

| Node | Provider | Tenant | Both | Only provider | Only tenant |
| --- | ---: | ---: | ---: | ---: | ---: |
| gpu-003 | 8,880 | 8,880 | 8,820 | 60 | 60 |
| gpu-005 | 3,600 | 3,720 | 3,600 | 0 | 120 |
| gpu-009 | 0 | 3,000 | 0 | 0 | 3,000 |
| gpu-011 | 7,200 | 7,200 | 7,200 | 0 | 0 |
| gpu-012 | 0 | 5,400 | 0 | 0 | 5,400 |
| gpu-014 | 14,400 | 14,400 | 14,400 | 0 | 0 |
| gpu-016 | 93,600 | 93,560 | 93,560 | 40 | 0 |

**Fault localisation** (`capledger-harness localize-sim --bad 2`)

| Nodes | 16 | 32 | 64 | 128 | 256 | 512 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Group tests | 15 | 19 | 23 | 27 | 31 | 35 |
| All pairs | 120 | 496 | 2,016 | 8,128 | 32,640 | 130,816 |

</details>

## What is in v0.3

| Component | Language | What it does | Status |
| --- | --- | --- | --- |
| `capledger sla compute / verify` | Go | Contract spec (YAML or JSON) + events → sealed SLA statement; source allow-lists and reconcile rules; independent replay | Unit, property and CLI tests; v0.1 and v0.2 hashes pinned; matched against an independent Python implementation on random months in CI |
| `capledger sla sign`, `sla verify --require` | Go | Each party signs a statement (accept or dispute); co-signing checked against keyring roles | Forgery and tamper tests |
| `capledger sla corroborate` | Go | Provider and tenant events for one contract → node-by-node agreement report | Worked example pinned by tests |
| `capledger ledger …` | Go, stdlib only | Hosted ledger per contract: verified pushes, witness checkpoints, statements and signatures, consented financier feed; push, pull, publish, consent, feed | End-to-end tests and demo; file-backed |
| `capledger evidence checkpoint`, `verify --checkpoint` | Go | Signed log heads that catch truncated or rewritten history | Rewrite and truncation tests |
| `capledger evidence / events / keyring` | Go | Ed25519-signed, hash-chained logs with a Merkle root; per-node keys; keyrings with owners, roles, node bindings and validity windows; heartbeat gap detection | Tamper, binding and gap tests |
| `capledger bundle` | Go | Signed `.tar.gz` evidence bundles; streaming verification; safe extraction | Tamper tests and fuzzing |
| `capledger agent` | Go, stdlib only | Kernel log → NVIDIA XID/SXID and AMD amdgpu observations; AMD RAS counters; GPU count; heartbeats → signed events | Tested on sample logs and a fake sysfs, not yet on GPU hosts |
| `capledger probe` | Go, stdlib only | Tenant side: GPU visibility and a benchmark floor with hysteresis → the customer's own signed events and heartbeats | Tested with simulated tool output |
| `deploy/slurm/slurm-state-watcher.sh` | Bash | Slurm drain and resume → signed down and up events | Tested against a fake `sinfo` |
| `capledger-harness plan / run / report` | Python | Acceptance plan (audit, DCGM or RVS, RDMA, NCCL or RCCL, burn-in, storage), runner, per-node verdicts | Tested with simulated tool output |
| `capledger-harness localize-sim / outliers` | Python | Group-testing fault localisation with a chosen fan-out; slow-node detection against peers | Tested in simulation |
| `capledger-harness pov-plan / pov-score`, `inject_fault.sh`, `agent_overhead.py` | Python, Bash | Proof-of-value kit: inject faults F1–F6 on test nodes and undo them, plan the trials, score against the roadmap's thresholds, measure agent overhead | Tested with fake tools; for rented test clusters only |
| `harness/scripts/burnin.py`, `gemm_bench.py` | Python, PyTorch | Simultaneous GEMM and all-to-all burn-in; per-GPU GEMM benchmark for the probe | Reference code; not yet run on hardware |

What changed in each version is in the [changelog](CHANGELOG.md).

## Quickstart

Requirements: Go 1.22+ and Python 3.10+. The Go code has no third-party dependencies.

```bash
git clone https://github.com/Sievehouse/gpu-sentinel.git && cd gpu-sentinel
make build                              # bin/capledger
python3 -m venv .venv && . .venv/bin/activate
pip install -e "harness[dev]"           # capledger-harness
make test                               # Go and Python tests
./scripts/demo.sh                       # end-to-end on synthetic data, a few seconds
```

### 1. Compute an SLA statement and replay it

```bash
./bin/capledger sla compute \
  --contract examples/contracts/hgx-16-node.yaml \
  --events   examples/events/hgx-16-node-2026-10.jsonl \
  --period 2026-10 --out statement.json --format text

./bin/capledger sla verify --statement statement.json \
  --contract examples/contracts/hgx-16-node.yaml \
  --events   examples/events/hgx-16-node-2026-10.jsonl
# replay OK: same inputs, same statement d6280aea... (engine semantics 0.1.0)
```

### 2. Record evidence on a node

```bash
./bin/capledger agent --node gpu-003 --source kmsg --log evidence-gpu-003.jsonl --key-dir keys/gpu-003
#   created signing key 655425e979f67bef in keys/gpu-003; heartbeats every minute
./bin/capledger event emit --log evidence-gpu-003.jsonl --key-dir keys/gpu-003 \
  --node gpu-003 --type up --cause node-reseated --source provider-health-check

./bin/capledger keyring import --keyring keyring.json --log evidence-gpu-003.jsonl \
  --owner "Example Neocloud" --role provider-agent        # trusts the key for gpu-003 only
./bin/capledger evidence verify --log evidence-gpu-003.jsonl --keyring keyring.json
./bin/capledger events export   --log evidence-gpu-003.jsonl --keyring keyring.json --derive-gaps 5m > events.jsonl
```

`--derive-gaps` turns silences in the heartbeats into unmonitored time, which every statement reports. For a monthly statement, export all nodes' logs together with `--period 2026-10 --contract …`, so that a node that sent no heartbeat at all is flagged too. The agent only reads: it never resets GPUs, drains nodes or touches the scheduler. On AMD hosts add `--vendor amd` ([AMD GPUs](docs/amd-gpus.md)).

### 3. Probe from the customer's side and compare

```bash
./bin/capledger probe --node gpu-003 --expected-gpus 8 --interval 60s \
  --log probe-gpu-003.jsonl --key-dir keys/probe-gpu-003

./bin/capledger sla corroborate --contract examples/contracts/hgx-16-node.yaml --period 2026-10 \
  --events-a examples/events/hgx-16-node-2026-10.jsonl \
  --events-b examples/events/hgx-16-node-2026-10-tenant.jsonl
```

The probe opens an incident only after several failed checks in a row and closes it only after several good ones, so one slow `nvidia-smi` call never becomes downtime (`--fail-after 3` and `--recover-after 2` by default):

```mermaid
stateDiagram-v2
  direction LR
  [*] --> Up
  Up --> Suspect: check fails
  Suspect --> Up: check passes
  Suspect --> Down: 3rd failure in a row, signed down event
  Down --> Recovering: check passes
  Recovering --> Down: check fails
  Recovering --> Up: 2nd pass in a row, signed up event
```

Ready-made jobs: [`deploy/kubernetes/tenant-probe-job.yaml`](deploy/kubernetes/tenant-probe-job.yaml) and [`deploy/slurm/tenant-probe.sbatch`](deploy/slurm/tenant-probe.sbatch).

### 4. Plan, run and sign an acceptance run

```mermaid
flowchart LR
  I["inventory<br/>nodes, GPUs, fabric"] --> PL["plan"]
  PL --> AU["audit<br/>nvidia-smi or amd-smi,<br/>topology, kernel"]
  AU --> SN["single node<br/>DCGM diag or RVS"]
  SN --> RD["RDMA<br/>perftest"]
  RD --> CO["collectives<br/>nccl-tests or rccl-tests"]
  CO --> BI["burn-in<br/>GEMM and all-to-all"]
  BI --> ST["storage<br/>fio"]
  ST --> RP["report<br/>per-node verdicts"]
  RP --> BD[["signed bundle"]]
  CO -. "floor missed" .-> LZ["group-testing<br/>localisation"]
```

```bash
capledger-harness plan --inventory examples/inventory/hgx-4-node.yaml --out plan.json
capledger-harness run --plan plan.json --out results.jsonl --dry-run     # print every command
capledger-harness run --plan plan.json --out results.jsonl --stage audit --stage single_node
capledger-harness report --results results.jsonl --cluster example-hgx-32gpu --expected-gpus 8 \
  --criteria examples/criteria/hgx-h100-400g.yaml --out report.json --markdown report.md
./bin/capledger bundle create --out acceptance.tar.gz --type acceptance --subject example-hgx-32gpu \
  --key-dir keys/harness --file report.json --file report.md --file results.jsonl --file plan.json
./bin/capledger bundle verify --bundle acceptance.tar.gz --pub keys/harness/capledger.pub.pem
capledger-harness localize-sim --nodes 64 --bad 2     # 2 bad nodes found in 23 tests, not 2,016 pairs
```

For AMD Instinct clusters, set `vendor: amd` in the inventory ([`examples/inventory/mi300x-2-node.yaml`](examples/inventory/mi300x-2-node.yaml)).

### 5. Agree on the month, through a ledger

```bash
# whose clock counts: version 2 of the example contract reconciles both parties' records
./bin/capledger sla compute --contract examples/contracts/hgx-16-node-reconciled.yaml \
  --events examples/events/hgx-16-node-2026-10-both.jsonl --period 2026-10 --format text   # 99.6946%

# each party signs; the statement counts as agreed only when both accept
./bin/capledger sla sign --statement statement.json --party customer --key-dir keys/customer-signer
./bin/capledger sla verify --statement statement.json --keyring keyring.json --require provider,customer \
  --signature statement.provider.sig.json --signature statement.customer.sig.json

# one ledger per contract: push logs, fetch the witness checkpoint, publish, let a lender read
./bin/capledger ledger serve --config ledger.json --keyring keyring.json --data data --key-dir witness
./bin/capledger ledger push --log evidence-gpu-003.jsonl --server https://ledger.example.com --token-file neocloud.token
./bin/capledger ledger checkpoint --out checkpoint.json --server … --token-file lab.token
./bin/capledger evidence verify --keyring keyring.json --checkpoint checkpoint.json --log pulled/gpu-003.jsonl
./bin/capledger ledger publish --statement statement.json --signature statement.customer.sig.json --server … --token-file lab.token
./bin/capledger ledger consent --financier example-insurer --party customer --key-dir keys/customer-signer --server … --token-file lab.token
```

The [ledger guide](docs/ledger-service.md) covers setting one up, day-to-day use, the API and operations; `./scripts/demo.sh` runs all of it on one machine.

## Design principles

| Principle | What it means |
| --- | --- |
| **Deterministic money path** | Same inputs, same statement, same hash, on any machine. Integer seconds and exact rationals only. |
| **Two vantage points** | The provider's agent and the customer's probe record the same nodes from opposite sides; corroboration shows exactly where they differ, so neither party has to trust the other's dashboard. |
| **Least privilege** | The agent and the probe only read, have no third-party dependencies and run without root on most hosts. Every writer has its own key, and a key can speak only for the nodes it is enrolled for. |
| **Silence is evidence too** | A collector that stops leaves a visible gap; the contract decides whether that gap counts as downtime. |
| **Reuse proven tools** | nccl-tests, rccl-tests, DCGM, the ROCm Validation Suite, perftest and fio do the measuring; GPU Sentinel orchestrates them, compares results and keeps the evidence. |
| **No AI in verdicts or credits** | A wrong number here moves money. Language models may later draft incident summaries, always linked to the signed records. |
| **Honest limits** | An acceptance report says "no defect found by these tests", never "no defect exists". |

## Documentation

| Topic | What it covers |
| --- | --- |
| [Architecture](docs/architecture.md) | Components, data flow and trust boundaries |
| [SLA spec](docs/sla-spec.md) | Contract format, every rule, worked examples by hand |
| [Keys and keyrings](docs/keys-and-keyring.md) | Per-node keys, enrolment, rotation |
| [Tenant probe](docs/tenant-probe.md) | Running the probe and reading a corroboration report |
| [AMD GPUs](docs/amd-gpus.md) | amdgpu messages, RAS counters, RVS and rccl-tests |
| [Acceptance tests](docs/acceptance-tests.md) | Stages, criteria and fault localisation |
| [Evidence formats](docs/evidence-format.md) | Log entries, bundles, checkpoints, signatures and consents, byte for byte |
| [Ledger service](docs/ledger-service.md) | Running the ledger, roles, checkpoints, co-signing, financier feed, API |
| [Proof of value](docs/proof-of-value.md) | Injecting faults on test clusters and scoring detection against the thresholds |
| [Threat model](docs/threat-model.md) | What the evidence does and does not prove |

<details>
<summary>Repository layout</summary>

```text
gpu-sentinel/
├── cmd/capledger/            CLI, one file per area: agent, probe, sla, evidence, bundle, keys, ledger
├── internal/
│   ├── agent/                kernel-log follower, fault handling, GPU count, heartbeats
│   ├── bundle/               signed evidence bundles
│   ├── canonical/            deterministic JSON for hashing
│   ├── contract/             contract SLA spec: load (YAML or JSON) and validate
│   ├── events/               availability events: load, normalise, order
│   ├── evidence/             signed hash-chained log, keys, announcements, Merkle root, checkpoints
│   ├── gaps/                 heartbeat silences → unmonitored time
│   ├── interval/             exact interval-set arithmetic
│   ├── keyring/              trusted keys: owners, roles, node bindings, validity
│   ├── ledger/               hosted ledger: configuration, file store, HTTP API, client, consents
│   ├── probe/                tenant-side GPU visibility and benchmark probe
│   ├── sla/                  SLA engine, reconcile rules, sealed statements, signatures, corroboration
│   ├── xid/                  NVIDIA XID/SXID and AMD amdgpu parsing, RAS counters, policy
│   └── yamlite/              strict YAML subset reader for contracts
├── harness/                  Python acceptance harness, proof-of-value kit, SLA reference implementation
├── examples/                 contracts, events, inventories, criteria, policies, sample tool output
├── deploy/                   agent DaemonSet, tenant probe job, ledger (systemd, StatefulSet), Dockerfile, Slurm
├── docs/                     architecture, spec, keys, probe, AMD, tests, ledger, proof, formats, threats, roadmap
│   └── images/               README images (regenerate with scripts/readme_images.py)
└── scripts/                  demo.sh, rename-module.sh, readme_images.py
```

</details>

> [!TIP]
> GPU Sentinel was called Capacity Ledger until v0.2. The command-line tool is still `capledger`, and file formats keep their `capacity-ledger/v1alpha1` identifiers and signing domains, so every existing log, statement, signature and hash stays valid.

## Roadmap

```mermaid
timeline
  title GPU Sentinel releases
  v0.1 (Oct 2026) : SLA engine with sealed, replayable statements
                  : Signed, hash-chained evidence logs
                  : NVIDIA XID agent, Slurm watcher, acceptance harness
  v0.2 (Oct 2026) : Heartbeats and unmonitored time
                  : Tenant probe and corroboration
                  : AMD Instinct, signed bundles, YAML contracts, keyrings
  v0.3 (Oct 2026) : Hosted ledger with provider, customer and financier roles
                  : Co-signed statements, witness checkpoints, consented feed
                  : Reconcile rules, source allow-lists, proof-of-value kit
  Next            : The technical proof on rented clusters
                  : PostgreSQL and object-storage backends
                  : Host attestation, hardware-backed keys
```

See [docs/roadmap.md](docs/roadmap.md), which also holds the 30-day validation plan this project is gated on. The next step is that proof, not more features.

## Contributing, security and licence

Issues and pull requests are welcome: see [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md). Licensed under the [Apache License 2.0](LICENSE).

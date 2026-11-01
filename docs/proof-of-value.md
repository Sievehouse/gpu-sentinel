# Running the proof of value

The [roadmap](roadmap.md) gates further building on a technical proof: rent two small clusters, inject known faults, and show that GPU Sentinel finds them better than standard practice, with few false alarms and little overhead. This page is the runbook, and the kit for it is in the repository.

> [!WARNING]
> Everything here degrades nodes on purpose. Use it only on clusters rented for the proof, never on production capacity, and tell the provider what you are doing: F6 makes the agent record a real `down` event.

## The pass thresholds

| Threshold | Target | How it is measured |
| --- | --- | --- |
| Recall for degradations of 10% or more | at least 95% | `capledger-harness pov-score`: share of injected nodes flagged (review or reject) |
| Recall for 5% degradations | at least 80% | the same, for 5% to 10% |
| False flags | at most 1 per 100 node-tests | flagged nodes that had nothing injected, over all clean node-tests (clean runs and the healthy nodes of faulty runs) |
| Localisation | culprit within 3 rounds | `rounds` per trial; use `--fanout 8` (or more for clusters above 64 nodes) |
| SLA engine | 100% match with independent references | the Python reference implementation in CI, plus the hand-worked examples in the [SLA spec](sla-spec.md) |
| Agent overhead | under 1% of one CPU core and 200 MB RAM | `harness/scripts/agent_overhead.py` on a node during a burn-in |

A threshold without data is reported as "no data", never as passed.

## 1. Plan the trials

```bash
capledger-harness pov-plan --inventory inventory.yaml --repeats 3 --seed 1 --out trials.jsonl
```

This writes the trial matrix from the roadmap: ten clean runs (F0), power caps at 5%, 10% and 20% (F1), locked clocks (F2), one rail down (F3), a wrong NCCL setting (F4), throttled storage (F5) and a synthetic XID drill (F6), each faulty trial repeated `--repeats` times with a victim node chosen at random (deterministically from `--seed`). Each line holds the trial's name, the victim, where its report will go, and the commands to inject and restore the fault:

```json
{"trial": "f1-10-r2", "fault": "F1", "injected": ["gpu-003"], "report": "runs/f1-10-r2/report.json", "magnitude": 10,
 "inject": ["sudo ./inject_fault.sh --yes --truth injections.jsonl --node gpu-003 power-cap 10"],
 "restore": ["sudo ./inject_fault.sh --yes --truth injections.jsonl --node gpu-003 restore"]}
```

Fill in the capitalised placeholders first: the base clock for F2, the RDMA interface of one rail for F3, a wrong GID index for F4, the job's cgroup, the storage device and a throttled rate for F5, and a GPU's PCI address for F6.

## 2. Run each trial

On the victim node, as root, inject the fault; from the head node, run the acceptance stages into the trial's directory; then restore:

```bash
sudo ./inject_fault.sh --yes --truth injections.jsonl --node gpu-003 power-cap 10
capledger-harness run --plan plan.json --out runs/f1-10-r2/results.jsonl
capledger-harness report --results runs/f1-10-r2/results.jsonl --cluster proof --expected-gpus 8 \
  --criteria criteria.yaml --out runs/f1-10-r2/report.json
sudo ./inject_fault.sh --yes --truth injections.jsonl --node gpu-003 restore
```

`inject_fault.sh` refuses to run without `--yes`, prints what it would do with `--dry-run`, keeps the undo commands in `/run/capledger-faults` (so `restore` works from another shell) and appends every injection and restore to the truth file. `status` lists what is applied. The faults:

| Fault | Command | What it does |
| --- | --- | --- |
| F1 | `power-cap PCT [GPU]` | `nvidia-smi -pl` at PCT% below each GPU's default limit |
| F2 | `lock-clocks MHZ [GPU]` | `nvidia-smi -lgc MHZ,MHZ` |
| F3 | `link-down IFACE` | `ip link set IFACE down` |
| F4 | `nccl-misconfig KEY=VALUE` | appends the setting to `/etc/nccl.conf` (backed up first) |
| F5 | `io-throttle CGROUP DEV BPS` | writes `DEV rbps=BPS wbps=BPS` to the cgroup's `io.max` |
| F6 | `xid-drill [XID] [PCI]` | writes a synthetic `NVRM: Xid` line to `/dev/kmsg`; the agent should record a `down` event within a second |

For F6, measure how long the provider's own health checks take to react compared with the agent, and record the recovery afterwards with `capledger event emit --type up --cause drill-end`.

If localisation runs in a trial, add its `rounds` to the trial's line. Run it with a fan-out that can meet the target: `capledger-harness localize-sim --nodes 64 --bad 2 --fanout 8` shows 3 rounds and 25 tests, against 7 rounds and 23 tests for plain halving.

## 3. Score

```bash
capledger-harness pov-score --trials trials.jsonl --out score.json
```

For example, on twenty made-up trials where one 5% power cap went unnoticed and one healthy node was flagged in a clean run:

```text
20 trial(s); verdict: FAIL

  recall, degradation >= 10%              1.0  (threshold 0.95)  pass
  recall, degradation 5-10%            0.6667  (threshold 0.8)  fail
  false flags per 100 node-tests       0.6623  (threshold 1.0)  pass
  localisation rounds (worst)               3  (threshold 3)  pass

  F1 GPU power cap                                8/9 detected
  missed: f1-5-r0 gpu-004 (F1, 5%)
  false flag: f0-x gpu-002
```

`--flag-level reject` counts only rejects as detections. Every miss and every false flag is listed by trial and node. The exit code is 1 if any threshold fails.

To compare with standard practice, score the provider's own health checks the same way: put the nodes they flagged in each trial's `flagged` list instead of a `report`.

## 4. Overhead and the SLA engine

During a burn-in, measure the agent on one node for an hour:

```bash
python3 harness/scripts/agent_overhead.py --pid "$(pidof capledger)" --seconds 3600
```

It prints the average CPU use as a share of one core and the peak resident memory, and exits 1 above 1% or 200 MB. On a development machine, following a log file with heartbeats every second (sixty times the default rate), the agent used 0.1% of a core and 10 MB over ten seconds; measure on your own hosts, under load.

The SLA engine threshold is covered without hardware: the Python reference implementation in `harness/src/capledger_harness/sla_reference.py` is compared with the engine in CI on every example and on 1,000 random months per run. To run more locally:

```bash
make build
cd harness && CAPLEDGER_BIN=../bin/capledger CAPLEDGER_DIFF_CASES=5000 pytest -q tests/test_sla_reference.py
```

## 5. A pilot: the month's statement against the provider's own figures

The proof above shows that GPU Sentinel finds faults. A pilot shows what that is worth: run the agents (and, if the customer agrees, the probe) on a few real nodes for a month, compute the statement, and compare it with the downtime the provider reports by its own means, such as its status page, its SLA report or its monitoring. Put the provider's figures in a CSV with one line per node:

```text
node,downtime_seconds
gpu-003,7200
gpu-016,28800
```

```bash
capledger sla compare --statement statement.json --provider-report provider-report.csv \
  --contract contract.yaml --tolerance 5m
```

With `examples/pilot/provider-report-2026-10.csv` and the October example statement:

```text
statement   availability 99.7021%, downtime 127680 node-seconds, credit 5% = 15000.00 USD
provider    availability 99.9160%, downtime 36000 node-seconds, credit 0% = 0.00 USD
Flagged     5 node(s) differ by more than 300 s

Node            statement   provider  difference
gpu-003              8880       7200       -1680  <-
gpu-005              3600          0       -3600  <-
gpu-011              7200          0       -7200  <-
gpu-014             14400          0      -14400  <-
gpu-016             93600      28800      -64800  <-
```

A negative difference means the provider reports less downtime than the signed evidence shows. Each flagged node is a question to ask: maintenance announced too late (gpu-011), a degraded node the status page does not count (gpu-014), an outage that started in the previous month (gpu-005), or a repair that took longer than the ticket says (gpu-016). The answers, not the tool, decide whether the gap is real; the statement's incidents cite the events behind every second. `--format json` gives the same comparison for a spreadsheet.

## What to keep

The truth file, every trial's results and report, the score, the overhead measurement and, for a pilot, the statement, the provider's report and the comparison. Pack them into one signed bundle with `capledger bundle create --type acceptance`, so the proof itself is evidence.

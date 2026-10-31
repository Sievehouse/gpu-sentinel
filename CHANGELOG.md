# Changelog

All notable changes to GPU Sentinel (called Capacity Ledger until 0.2). Before version 1.0, minor versions may extend formats; every such change is listed here, and existing logs, statements and contracts keep working.

## 0.4.0 (2026-11-01)

### Added

- **Kubernetes and NVSentinel.** `capledger watch kubernetes` turns Kubernetes node health into signed down, up and degraded events: a `Ready` condition that is not `True`, health conditions such as those of NVIDIA NVSentinel and node-problem-detector (`--down-condition '*'`), taints, cordons and fewer allocatable GPUs than expected. Events are dated by the condition's own transition time, contract nodes missing from the cluster count as down, and the watcher resumes from its own log after a restart. It reads nodes with the pod's service account, with kubectl, from an API server URL or from a saved `kubectl get nodes -o json` file (`--at` replays saved views). `deploy/kubernetes/node-watcher.yaml` runs it in the cluster with read-only access; `examples/kubernetes/` holds two saved views; [docs/kubernetes.md](docs/kubernetes.md) explains the rules.
- **Redaction.** `capledger agent --redact` removes process names and ids, which can identify a customer's jobs, from recorded kernel lines; `xid parse --redact` shows what would be recorded. Classification uses the original line, so the same events come out, and redacted observations carry `"redacted": true`.
- **One ledger, several contracts.** `ledger serve --config-dir DIR` (or `--config` given more than once) hosts several contracts, each with its own configuration, tokens, data directory and API under `/c/CONTRACT`. A configuration's new optional `keyring` field gives the contract its own keyring. SIGHUP adds and removes contracts; each checkpoint names its contract.
- **Windows.** `scripts/demo.ps1` runs the end-to-end demo in Windows PowerShell 5.1 or PowerShell 7, and CI runs the Go tests and that demo on Windows.

### Changed

- The SLA engine is unchanged (still 0.3.0): every statement keeps its hash.
- With a single `--config`, `ledger serve` behaves as before; `--keyring` is now needed only for configurations that name no keyring of their own.

### Upgrading from 0.3

- Nothing changes for existing logs, statements, contracts and ledgers. To host more contracts on one ledger, move the configurations into one directory, give each a `keyring`, and point the parties of each contract at `https://HOST/c/CONTRACT`. A ledger started with `--config-dir` keeps each contract's data under `DATA/contracts/CONTRACT`, so move an existing single-contract data directory there first.
- To admit the Kubernetes watcher's events under a contract with `sources`, list the role `provider-integration` (or the source `kubernetes-node-watcher`) for the provider.

## 0.3.0 (2026-10-31)

The project is now called GPU Sentinel. The command is still `capledger`, and file formats keep their `capacity-ledger` identifiers, so every existing log, statement, bundle and signature stays valid.

### Added

- **Whose clock wins.** Contracts can list their event sources by party (`sources.provider`, `sources.tenant`: keyring roles or source names; `unlisted: reject | ignore`) and say how the provider's and the customer's records of the same outage combine (`measurement.reconcile`: which episodes count, and whose clock starts and ends them). Every incident in a reconciled statement says who saw it. Version 2 of the example contract and `examples/events/hgx-16-node-2026-10-both.jsonl` are worked through by hand in the SLA spec.
- **Signer attestation.** `events export --signers` adds each event's signing key and its keyring role, which contract sources match on. Signer fields in a payload are always dropped.
- **Co-signed statements.** `capledger sla sign` records one party's `accept` or `dispute` of a statement; `sla verify --signature … --keyring K --require provider,customer` checks that every required party accepted. A key signs only as its keyring role.
- **Checkpoints.** `capledger evidence checkpoint` signs the size, last hash and Merkle root of logs; `evidence verify --checkpoint` fails a log that was truncated or rewritten since, even if its writer re-signed every entry.
- **The hosted ledger.** `capledger ledger serve` runs one ledger per contract (standard library only, file-backed): verified pushes, one owner per log, daily witness checkpoints, statements with every party's signature, and a feed for financiers that opens only while both the provider and the customer consent. Client commands: `ledger token`, `push`, `pull`, `checkpoint`, `publish`, `consent`, `feed` and `status`. Deployment files for systemd and Kubernetes are in `deploy/`.
- **Proof-of-value kit.** `harness/scripts/inject_fault.sh` injects and undoes the roadmap's faults F1 to F6 on test nodes; `capledger-harness pov-plan` and `pov-score` plan the trials and score them against the pass thresholds; `harness/scripts/agent_overhead.py` measures the agent's CPU and memory. `localize-sim --fanout` trades tests for rounds.
- **Independent SLA reference.** A Python implementation of the SLA rules, written from the spec, is compared with the engine in CI on every example and on 1,000 random months per run.
- New keyring roles for party keys: `provider`, `customer`, `financier` and `witness`.

### Changed

- Engine 0.3.0. Statements under contracts with a `sources` block are stamped with semantics 0.3.0; all others stay byte-identical to engines 0.1 and 0.2, and `sla verify` replays all three.
- `sla corroborate --events` takes both parties' events in one set and splits them by the contract's sources. Under a reconciling contract each side is compared on its own record.
- `contract check` shows sources and reconcile rules.
- The demo starts a local ledger and runs the whole month through it.
- The roadmap's 0.4 items (a consented financier feed, contract-level allow-lists of sources) shipped in this release.

### Fixed

- Container image names are lowercase again (`ghcr.io/sievehouse/gpu-sentinel-agent`); the module rename had made them invalid.

### Upgrading from 0.2

- Nothing changes for existing contracts, logs and statements. To use reconcile rules, add `sources` and `measurement.reconcile` to a new contract version, and export events with `--signers` if the sources list roles.
- Enrol each party's signing key with its party role (`keyring add --role provider`) before signing statements; do not reuse node keys for this.

## 0.2.0 (2026-10-25)

### Added

- **Heartbeats and gap detection.** The agent and the probe write signed heartbeats (`--heartbeat`, default one minute). `capledger evidence gaps` lists silences, and `capledger events export --derive-gaps 5m` turns them into `unmonitored_start` / `unmonitored_end` events.
- **Unmonitored time in statements.** Contracts take `measurement.unmonitored: report | count-as-down`, and statements list unmonitored time per node and in total.
- **Tenant probe.** `capledger probe` checks, from inside the customer's allocation, that every GPU is visible (NVIDIA or AMD) and optionally that a benchmark stays above a floor, with hysteresis, and writes the customer's own signed log. Examples for Kubernetes and Slurm are in `deploy/`.
- **Corroboration.** `capledger sla corroborate` compares the downtime that two event sets record for the same contract and period, node by node, and seals the report.
- **AMD Instinct.** `capledger agent --vendor amd` parses amdgpu kernel messages (RAS errors, GPU resets, ring timeouts, page faults), reads RAS error counters from sysfs and counts GPUs bound to the amdgpu driver. Fault policies gain an `amd` section. Harness inventories take `vendor: amd` (amd-smi audit, ROCm Validation Suite, rccl-tests) and `single_node_command`.
- **Signed evidence bundles.** `capledger bundle create` and `capledger bundle verify [--extract]` freeze an acceptance report and its raw results, or a statement and its inputs, under one signature.
- **YAML contracts.** The engine reads YAML directly (a strict subset with line-numbered errors). `capledger contract check` validates a contract and prints its digest.
- **Per-node keys and keyrings.** Writers take `--key-dir`, create their own key there on first use (the agent defaults to `/var/lib/capledger/keys`) and announce it in their log. `capledger keyring add | import | list` record who holds each key, its role, the nodes it may speak for and when it is valid; `--keyring` on every verifying command enforces all of it.

### Changed

- Engine 0.2.0. Statements record the oldest semantics that reproduce them: statements without unmonitored time are byte-identical to engine 0.1.0 and keep their hashes, and `sla verify` replays both.
- The CLI is split into one file per area. Command-line mistakes now exit with code 2 (verification failures still exit with 1), and stray positional arguments are rejected.
- The DaemonSet uses per-node keys instead of a shared secret, prepares its host directory with an init container and turns heartbeats on.
- The Slurm watcher signs with its own key directory (`CAPLEDGER_KEY_DIR`) unless `CAPLEDGER_KEY` names a key file.
- Harness 0.2.0. The criteria field `require_dcgm_pass` now covers every single-node diagnostic.

### Fixed

- The agent's GPU count no longer counts nvidia-smi error lines ("Unable to determine the device handle …") as GPUs, which could hide a lost GPU. The harness stops counting GPUs at an audit's first `--- ` section.
- Statement text rows no longer end in spaces.

### Upgrading from 0.1

- `--key` and `--pub` work as before, and so do existing logs, statements and contracts.
- An agent started without `--key` now uses `/var/lib/capledger/keys` and creates a key there. With the new DaemonSet, each agent keeps appending to its existing log with its new key. Enrol the new keys with `capledger keyring import`, add the old shared key with `capledger keyring add --pub old.pub.pem … --not-after <switch-over time>` so that the history still verifies, then delete the old `capledger-agent-key` secret.
- Scripts that treated exit code 1 as "bad command line" should check for 2.

## 0.1.0 (2026-10-22)

First public version: SLA engine with sealed, replayable statements; signed, hash-chained evidence logs with a Merkle root; NVIDIA XID/SXID agent; Slurm state watcher; Python acceptance harness with fault localisation and peer comparison; examples and documentation.

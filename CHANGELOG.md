# Changelog

All notable changes to Capacity Ledger. Before version 1.0, minor versions may extend formats; every such change is listed here, and existing logs, statements and contracts keep working.

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

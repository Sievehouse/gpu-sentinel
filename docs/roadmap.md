# Roadmap

Building is gated on customer evidence, not the other way round. The 30-day plan below comes from the opportunity research behind this project; the build milestones only start in earnest after Gate 4.

## 30-day validation plan

| Days | Stage | Work | Gate: proceed if | Stop if |
| --- | --- | --- | --- | --- |
| 1–7 | Discovery | Finish desk research; book 18 interviews (6 neoclouds, 5 labs, 2 marketplaces, 2 financiers, assessors, security); map partner stance; cost the proof | 15+ interviews booked, including 2 financiers; proof costed under $40,000 | Fewer than 8 interviews bookable, or a competitor launches a continuous SLA ledger |
| 8–14 | Competition and customers | Run 12+ interviews; test rival tools; meet assessors; price a pilot | 3 contacts share real SLA terms or logs; 1 neocloud names a costed trigger | Nobody can describe a recent, costly acceptance or SLA dispute |
| 15–21 | Technical proof | Rent two small clusters; inject faults F0–F6 (see below); score against standard practice; verify the SLA engine | Detection and localisation thresholds met on at least one provider | Cannot beat standard practice after one iteration |
| 22–30 | Commercial validation | Show results; ask for paid pilots; financier interview | 3 commitments to a pilot, including 1 paid; 1 financier states a data requirement | Fewer than 3 commitments or no paid signal |

Interview evidence ladder: 0 positive feedback, 1 a specific recent problem with a cost, 2 agrees to a test, 3 shares real data, 4 commits engineering time or signs a pilot, 5 pays. Only levels 4 and 5 count toward Gate 4.

## Proof-of-value faults

| ID | Injected fault | Simulates |
| --- | --- | --- |
| F0 | None, ten clean runs | False positives, test-retest variance |
| F1 | GPU power cap at −5%, −10%, −20% | Fail-slow GPU |
| F2 | GPU clocks locked below base | Thermal throttling |
| F3 | One node excludes one RDMA rail | Degraded link |
| F4 | Wrong RDMA GID index or missing NCCL plugin on one node | Configuration error |
| F5 | Storage client throttled with cgroup io.max | Degraded data path |
| F6 | DCGM error injection or synthetic XID, where permitted | Health-check timing drill |

Pass thresholds: at least 95% recall for degradations of 10% or more, at least 80% for 5%, at most 1 false flag per 100 node-tests, culprit found within 3 rounds, SLA engine 100% match with independent references, passive agent under 1% of one CPU core and 200 MB RAM.

## Build milestones

| Version | Scope |
| --- | --- |
| 0.1 | SLA engine, evidence log, XID agent, Slurm watcher, acceptance harness, examples |
| 0.2 (this) | Heartbeats and gap detection; tenant-side probe and provider/tenant corroboration; AMD collector (amdgpu kernel messages, RAS counters) and AMD acceptance runs; signed evidence bundles; engine reads YAML specs directly; per-node keys by default, with a keyring of owners, roles and node bindings |
| 0.3 | Hosted ledger service (PostgreSQL + object storage), provider, customer and financier roles, co-signed monthly statements, daily Merkle-root exchange |
| 0.4 | Consented financier feed for SLA insurers and lenders; contract-level allow-lists of event sources |

## Deliberately not planned

Remediation (leave it to NVSentinel or the provider's tooling), scheduling, invoicing, general dashboards, public provider rankings, and AI-driven pass/fail decisions.

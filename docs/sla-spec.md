# Contract SLA spec and engine semantics

The engine reads a contract spec (`apiVersion: capacity-ledger/v1alpha1`, `kind: ContractSLA`) and a set of availability events, and produces a sealed statement for one period.

Contracts can be written in YAML or JSON; `examples/contracts/hgx-16-node.yaml` and `hgx-16-node.json` are the same contract and have the same digest. The engine reads a strict subset of YAML: block and flow mappings and sequences, plain and quoted scalars, comments. Anchors, aliases, tags, multi-line strings, multiple documents and duplicate keys are rejected with a line number, so the file means exactly what it says. In YAML, decimals such as `availabilityBelow: 99.9` keep their literal text whether quoted or not. `capledger contract check --contract c.yaml` validates a contract and prints its digest.

## Fields

| Field | Meaning |
| --- | --- |
| `metadata.id`, `metadata.version` | Contract identity. Bump the version whenever terms change. |
| `cluster.gpusPerNode` | GPUs per node, used for GPU-hours. |
| `cluster.nodes` | Every covered node. Events for other nodes are ignored with a warning. |
| `cluster.racks[]` | Optional groups of nodes that share a scale-up domain (e.g. an NVL72 rack). |
| `measurement.minIncident` | Go duration. Countable outages shorter than this are dropped; outages of exactly this length count. |
| `measurement.degradedCountsAsDown` | Whether `degraded_start` / `degraded_end` intervals count as downtime. |
| `measurement.rackRule.nodesDownThreshold` | While at least this many nodes of a rack have countable downtime, every node of the rack is down. |
| `measurement.unmonitored` | What time without heartbeats means: `report` (the default) lists it in the statement without changing availability; `count-as-down` makes it countable downtime, subject to the same exclusions and minimum incident length as any outage. |
| `exclusions.maintenanceNotice` | Maintenance announced at least this long before it starts is excluded. Empty means maintenance always counts. |
| `exclusions.maxExcludedPerNode` | Cap on excluded time per node per period; time beyond it counts again. Empty means no cap. |
| `exclusions.customerCaused` | Exclude downtime whose every cause starts with `customer`. |
| `credits.tiers[]` | `availabilityBelow` (decimal string) → `creditPercent`. The highest applicable tier wins. |
| `credits.capPercent` | Cap on the credit; 0 means no cap. |
| `credits.monthlyFee`, `credits.currency` | Optional; turns the percentage into an amount. |

Unknown fields are rejected, and decimals must be plain strings such as `"99.9"`, so a typo or a float cannot silently change the result.

## Events

```json
{"id":"e-0003","ts":"2026-10-03T04:12:00Z","node":"gpu-003","type":"down","cause":"xid:79","source":"capledger-agent"}
```

Types: `down`, `up`, `degraded_start`, `degraded_end`, `maintenance_start` (with `noticeTs`), `maintenance_end`, `unmonitored_start`, `unmonitored_end`. Timestamps are RFC 3339 and truncated to whole seconds.

Unmonitored events are not written by hand. `capledger events export --derive-gaps 5m` derives them from the heartbeats in the evidence logs: a stretch in which a node sent no heartbeat for longer than the tolerated silence starts when the next heartbeat was due (the last heartbeat plus its promised interval) and ends at the next heartbeat. With `--period` (or `--since` / `--until`) and `--contract`, silence before the first and after the last heartbeat, and nodes that never sent one, are flagged too. The ids are deterministic (`gap:<node>:<start>`), so the same logs always export the same events. Events are processed in order of time, then `seq`, then `id`; producers that write several events for one node in the same second set `seq` (the CLI and agent use the evidence sequence number).

Rules worth knowing:

- An `up` closes the node's open outage, however many `down` events preceded it. Repeated `down` events merge into the open outage (with a warning count).
- An outage still open at the end of the period counts to the end; one that started before the period counts from its start.
- `up` without an open outage is ignored with a warning. The same holds for the ends of degraded, maintenance and unmonitored stretches.

## Order of operations (per node)

1. Replay events into down, degraded and maintenance intervals, clipped to the period.
2. Classify: customer-caused downtime and properly announced maintenance are **excluded**; everything else is **countable**.
3. Split excluded time at the cap: the first `maxExcludedPerNode` seconds, in time order, stay excluded; the rest counts.
4. Countable time = (countable − allowed exclusions) ∪ cap overflow; then drop outages shorter than `minIncident`. Where a failure overlaps an excluded window, the overlap is excluded.
5. Rack rule: while at least `nodesDownThreshold` nodes of a rack have countable downtime, all nodes of the rack are down. These stretches appear as incidents `via: rack:<name>` and cite the events that triggered them.
6. Cluster availability = 1 − (Σ node downtime) / (nodes × period), as an exact fraction, printed with four decimals. The credit is the highest tier whose threshold is above that availability, then capped.

Unmonitored time is always listed per node (`unmonitoredSeconds` and the stretches, with the events that define them) and in the summary (`unmonitoredNodeSeconds`). Under `count-as-down` it also enters step 2 as countable downtime with the cause `no-heartbeat`, so announced maintenance and the exclusion cap still apply to it.

## Worked example: `examples/contracts/hgx-16-node.yaml`, October 2026

Period: 31 days = 2,678,400 s per node; 16 nodes = 42,854,400 node-seconds.

| Node | Events | Result |
| --- | --- | --- |
| gpu-005 | down Sep 30 23:00 (xid:48), up Oct 1 01:00 | 3,600 s counted (from the period start) |
| gpu-003 | down Oct 3 04:12 (xid:79), up 06:40 | 8,880 s counted |
| gpu-007 | down Oct 9 10:00, up 10:03 | 3 min < 5 min minimum: not counted |
| gpu-010 | maintenance Oct 14 02:00–05:00, announced 98 h earlier | 10,800 s excluded |
| gpu-011 | maintenance Oct 20 01:00–03:00, announced 5 h earlier | 7,200 s counted (notice under 72 h) |
| gpu-012 | down Oct 22 08:00–09:30, cause `customer:oom-reboot` | 5,400 s excluded |
| gpu-014 | degraded Oct 25 12:00–16:00 | 14,400 s counted |
| gpu-016 | down Oct 30 22:00, no up | 93,600 s counted (to the period end) |

Downtime = 127,680 node-seconds → availability = 1 − 127,680 / 42,854,400 = 99.7021%. That is below 99.9 but not below 99.5, so the credit is 5% of $300,000 = $15,000.00. GPU-hours = 127,680 × 8 / 3,600 = 283.73.

The statement hash is `d6280aea2086f9bf153922421dffbc4d44fbe17cc1106ba5184cad29f99323fc`, the same as with engine 0.1.0.

## Worked example: unmonitored time, October 2026

`examples/events/hgx-16-node-2026-10-gaps.jsonl` adds three stretches without heartbeats to the events above:

| Node | Unmonitored | Seconds |
| --- | --- | --- |
| gpu-008 | Oct 12 03:00 to Oct 14 03:00, nothing else recorded | 172,800 |
| gpu-010 | Oct 14 02:10 to 04:40, inside the announced maintenance window 02:00–05:00 | 9,000 |
| gpu-016 | Oct 30 22:05 to the period end, during the open rail-3 outage | 93,300 |

With `hgx-16-node.yaml` (policy `report`) nothing countable changes: downtime stays 127,680 node-seconds, availability 99.7021% and the credit 5%. The statement lists 275,100 unmonitored node-seconds and, because it uses this 0.2 feature, records engine semantics 0.2.0.

With `hgx-16-node-strict.yaml` (`unmonitored: count-as-down`):

- gpu-008: 172,800 s become downtime with the cause `no-heartbeat`.
- gpu-010: the unmonitored stretch lies inside announced maintenance, which is excluded (10,800 s, under the 8 h cap), so it adds nothing.
- gpu-016: the stretch overlaps the outage that is already counted from 22:00; the union is still 93,600 s, now with both causes.

Downtime = 127,680 + 172,800 = 300,480 node-seconds → availability = 1 − 300,480 / 42,854,400 = 99.2988%. That is below 99.5 but not below 99.0, so the credit is 10% of $300,000 = $30,000.00. GPU-hours = 300,480 × 8 / 3,600 = 667.73.

## Worked example: `examples/contracts/nvl72-2-racks.json`, October 2026

Trays 03 (10:00–12:00), 11 (10:15–11:15) and 07 (10:30–11:45) of rack-a overlap. Three or more are down from 10:30 to 11:15, so all 18 trays of rack-a are down for those 45 minutes.

- Faulty trays: 7,200 + 3,600 + 4,500 s. The 15 healthy trays: 2,700 s each = 40,500 s.
- rack-b tray 02 is down 20 minutes on its own: 1,200 s, below the rack threshold.
- Total 57,000 node-seconds of 96,422,400 → 99.9409% → below 99.95 → 3% of $650,000 = $19,500.00.

## Engine versions and replay

Every statement records the engine semantics it needs in `engineVersion`: the oldest semantics that reproduce it. Engine 0.2.0 computes statements that use no 0.2 feature (no unmonitored time and no `measurement.unmonitored` in the contract) byte for byte as engine 0.1.0 did, and stamps them `0.1.0`. The hgx-16-node statement above therefore keeps its version 0.1 hash, `d6280aea…`, whether the contract is read from YAML or JSON. Statements that use unmonitored time are stamped `0.2.0`.

`capledger sla verify` replays statements of every semantics the binary supports (0.1.0 and 0.2.0 for engine 0.2.0) and refuses others rather than comparing them silently.

## Tests that pin these semantics

`internal/sla/engine_test.go` checks the worked examples, the version 0.1 statement hashes, determinism under shuffled input, replay and tamper detection, the exclusion cap and minimum-incident boundary, both unmonitored policies, the refusal of unknown semantics, and two monotonicity properties (an independent outage always lowers availability; an extra `down` event can never raise it). `internal/gaps` pins how heartbeats become gaps.

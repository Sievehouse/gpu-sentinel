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
| `sources.provider`, `sources.tenant` | Since 0.3. The event sources the contract accepts, by party: `roles` (keyring roles of the keys that signed the events, attested by `events export --signers`) and `sources` (self-declared source names). See [sources and reconcile rules](#sources-and-reconcile-rules-which-clock-wins). |
| `sources.unlisted` | `reject` (the default): an event from any other source stops the computation. `ignore`: such events are skipped and counted in the statement's warnings. |
| `measurement.reconcile.count` | Since 0.3, required when `sources.tenant` is set. Which episodes of downtime count: `either`, `both`, `provider` or `tenant`. |
| `measurement.reconcile.start`, `.end` | Whose clock starts and ends a counted episode: `earliest`, `latest`, `provider` or `tenant`. |

Unknown fields are rejected, and decimals must be plain strings such as `"99.9"`, so a typo or a float cannot silently change the result.

## Events

```json
{"id":"e-0003","ts":"2026-10-03T04:12:00Z","node":"gpu-003","type":"down","cause":"xid:79","source":"capledger-agent"}
```

Types: `down`, `up`, `degraded_start`, `degraded_end`, `maintenance_start` (with `noticeTs`), `maintenance_end`, `unmonitored_start`, `unmonitored_end`. Timestamps are RFC 3339 and truncated to whole seconds.

Events exported with `capledger events export --keyring K --signers` also carry `signerKey` and `signerRole`: the key that signed the evidence entry and its role in the verifier's keyring. They are never taken from a payload, so a writer cannot vouch for itself, and without `--signers` the export is byte-for-byte what version 0.2 produced.

Unmonitored events are not written by hand. `capledger events export --derive-gaps 5m` derives them from the heartbeats in the evidence logs: a stretch in which a node sent no heartbeat for longer than the tolerated silence starts when the next heartbeat was due (the last heartbeat plus its promised interval) and ends at the next heartbeat. With `--period` (or `--since` / `--until`) and `--contract`, silence before the first and after the last heartbeat, and nodes that never sent one, are flagged too. The ids are deterministic (`gap:<node>:<start>`), so the same logs always export the same events. Events are processed in order of time, then `seq`, then `id`; producers that write several events for one node in the same second set `seq` (the CLI and agent use the evidence sequence number).

Rules worth knowing:

- An `up` closes the node's open outage, however many `down` events preceded it. Repeated `down` events merge into the open outage (with a warning count).
- An outage still open at the end of the period counts to the end; one that started before the period counts from its start.
- `up` without an open outage is ignored with a warning. The same holds for the ends of degraded, maintenance and unmonitored stretches.

## Order of operations (per node)

1. Replay events into down, degraded and maintenance intervals, clipped to the period. Under reconcile rules, each party's events are replayed separately.
2. Classify: customer-caused downtime and properly announced maintenance are **excluded**; everything else is **countable**. Under reconcile rules, the two parties' countable time is then combined into one ([below](#sources-and-reconcile-rules-which-clock-wins)); excluded time from either party is pooled.
3. Split excluded time at the cap: the first `maxExcludedPerNode` seconds, in time order, stay excluded; the rest counts.
4. Countable time = (countable − allowed exclusions) ∪ cap overflow; then drop outages shorter than `minIncident`. Where a failure overlaps an excluded window, the overlap is excluded.
5. Rack rule: while at least `nodesDownThreshold` nodes of a rack have countable downtime, all nodes of the rack are down. These stretches appear as incidents `via: rack:<name>` and cite the events that triggered them.
6. Cluster availability = 1 − (Σ node downtime) / (nodes × period), as an exact fraction, printed with four decimals. The credit is the highest tier whose threshold is above that availability, then capped.

Unmonitored time is always listed per node (`unmonitoredSeconds` and the stretches, with the events that define them) and in the summary (`unmonitoredNodeSeconds`). Under `count-as-down` it also enters step 2 as countable downtime with the cause `no-heartbeat`, so announced maintenance and the exclusion cap still apply to it.

## Sources and reconcile rules ("which clock wins")

A provider's agent and a customer's probe record the same outage from opposite sides, and their clocks differ: the provider sees the XID at 04:12, the customer's job notices at 04:13; the provider's health check closes the incident at 06:40, the customer's job sees all GPUs again at 06:41. Both records are defensible. Comparing them settles nothing until the contract says which one counts, so version 0.3 lets the contract say it, before anyone compares records.

**Sources.** `sources.provider` and `sources.tenant` list who speaks for each party. An event that carries a `signerRole` belongs to the party whose `roles` include it, and its self-declared `source` name is then not considered: a provider key cannot pass its events off as the tenant's by writing `"source": "tenant-probe"`. An event without a signer role is matched by its `source` name. A contract that lists only roles therefore accepts only events exported from verified, signed logs. A role or name may belong to one party only. Events from unlisted sources stop the computation unless `unlisted: ignore`.

A contract may list only `sources.provider`: that is an allow-list and nothing more. Once it lists `sources.tenant`, the tenant's records count too, and `measurement.reconcile` is required.

**Reconcile.** The engine replays each party's events on its own and finds each party's countable time on the node. An *episode* is a stretch of the union of both: a time during which at least one party recorded the node as unavailable without interruption. For every episode:

- `count` decides whether it counts at all: `either` (if either party recorded it), `both` (only if both did), `provider` or `tenant` (only if that party did).
- `start` picks its start: `earliest` (the first record of either party), `latest` (the later of the two parties' first records), `provider` or `tenant` (that party's first record).
- `end` picks its end the same way round: `latest`, `earliest`, `provider` or `tenant` (for example the moment the tenant's jobs saw the node again).
- A rule that names a party which recorded nothing in the episode falls back to the other party's clock. An episode whose chosen end is not after its chosen start is dropped.

Exclusions are applied after this, to the combined time, whichever party recorded them: maintenance announced in time, customer-caused downtime and the exclusion cap work exactly as before. If the parties disagree about a cause (the provider records `customer:oom-reboot`, the probe saw the GPUs missing), the exclusion stands and the disagreement shows up, with its exact window, in `capledger sla corroborate`. Every incident in a reconciled statement says who saw it: `seenBy` is `both`, `provider` or `tenant`.

## Worked example: `examples/contracts/hgx-16-node-reconciled.yaml`, October 2026

Version 2 of the example contract adds sources and the rule *count what either party recorded, from the provider's first fault record to the tenant's recovery* (`count: either`, `start: provider`, `end: tenant`). `examples/events/hgx-16-node-2026-10-both.jsonl` holds both parties' records.

| Node | Provider | Tenant | Result |
| --- | --- | --- | --- |
| gpu-003 | down 04:12–06:40 | down 04:13–06:41 | 04:12 to 06:41: 8,940 s, seen by both |
| gpu-005 | down Sep 30 23:00 to Oct 1 01:00 | down 23:01 to 01:02 | period start to 01:02: 3,720 s |
| gpu-007 | down 10:00–10:03 | nothing | 3 min, under the 5 min minimum: not counted |
| gpu-009 | nothing | 6 of 8 GPUs, 07:00–07:50 | the tenant's clock is the only one: 3,000 s, seen by the tenant |
| gpu-010 | maintenance 02:00–05:00, announced 98 h ahead | probe errors 02:00:30–05:00:30 | the 30 s after the excluded window is under the minimum: 0 s; 10,800 s excluded |
| gpu-011 | maintenance 01:00–03:00, announced 5 h ahead | probe errors 01:00–03:00 | 7,200 s, seen by both |
| gpu-012 | down 08:00–09:30, `customer:oom-reboot` | 0 of 8 GPUs, 08:00–09:30 | customer-caused in the provider's record: excluded, 5,400 s |
| gpu-014 | degraded 12:00–16:00 | benchmark below floor 12:00–16:00 | 14,400 s, seen by both |
| gpu-016 | down Oct 30 22:00, no up | down 22:00:40, no up | 22:00 to the period end: 93,600 s |

Downtime = 130,860 node-seconds → availability = 1 − 130,860 / 42,854,400 = 99.6946%, still in the 5% tier: $15,000.00. The statement hash is `793920cf0be5f2c1e7ce83614657a28ae27daf291a68e56bbd4857e470879f73`, stamped with engine semantics 0.3.0.

The same records under other rules (`internal/sla/reconcile_test.go` pins each):

| `count`, `start`, `end` | Meaning | Downtime (node-seconds) |
| --- | --- | --- |
| `both`, `latest`, `earliest` | only what both parties saw, while both saw it | 127,580 (exactly the "seen by both" of the corroboration report) |
| `either`, `earliest`, `latest` | anything either party saw, for as long as anyone saw it | 130,860 |
| `either`, `provider`, `tenant` | the example: provider's first fault to the tenant's recovery | 130,860 |
| `provider`, `provider`, `provider` | the provider's record alone, as in version 1 | 127,680 |
| `tenant`, `tenant`, `tenant` | the tenant's clock alone (the provider's customer-caused exclusion still applies) | 130,760 |

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

Engine 0.3.0 keeps both promises: statements under contracts without a `sources` block are byte-identical to those of engines 0.1 and 0.2, and are stamped as before. Statements under contracts with `sources` are stamped `0.3.0`.

`capledger sla verify` replays statements of every semantics the binary supports (0.1.0, 0.2.0 and 0.3.0 for engine 0.3.0) and refuses others rather than comparing them silently.

## Signing a statement

A sealed statement proves what was computed, not that anyone agreed to it. `capledger sla sign` records one party's decision about a statement (`accept`, or `dispute` with a note) and signs it with the party's own key; `capledger sla verify --signature ... --keyring K --require provider,customer` passes only if each required party's latest valid signature accepts the statement. The party a key may sign for is its role in the verifier's keyring, so the provider's key cannot sign as the customer. The signature format is in [evidence formats](evidence-format.md#statement-signatures).

## Tests that pin these semantics

`internal/sla/engine_test.go` checks the worked examples, the version 0.1 statement hashes, determinism under shuffled input, replay and tamper detection, the exclusion cap and minimum-incident boundary, both unmonitored policies, the refusal of unknown semantics, and two monotonicity properties (an independent outage always lowers availability; an extra `down` event can never raise it). `internal/sla/reconcile_test.go` pins the reconcile rules case by case, the reconciled example and its rule variants, and that a signer role decides the party. `internal/gaps` pins how heartbeats become gaps.

The harness holds an independent implementation of these rules in Python, written from this document rather than from the Go code (`harness/src/capledger_harness/sla_reference.py`). CI compares it with the engine on every example and on 1,000 random months per run (random contracts, rules and events, including duplicates, unpaired transitions and unknown nodes); every node's downtime, exclusions, unmonitored time, the availability and the credit must match exactly.

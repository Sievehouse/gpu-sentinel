# Ticket systems

Repairs and maintenance usually live in a ticket system (Jira, ServiceNow, PagerDuty, Opsgenie or an in-house tool) long before anyone writes an SLA event by hand. `capledger watch webhook` receives that system's webhooks and records them as signed events: a ticket that moves to "In Repair" opens an outage, and "Repaired" closes it. Like the Slurm and Kubernetes watchers, it records what the provider's own tooling says and decides nothing about the contract.

## The mapping

One JSON file says where things are in the delivered JSON and which statuses mean what (`examples/webhooks/jira-repairs.json`):

```json
{
  "apiVersion": "capacity-ledger/v1alpha1",
  "kind": "WebhookMapping",
  "source": "jira-repairs",
  "auth": {"hmacSecretFile": "jira-webhook-secret.txt", "hmacHeader": "X-Hub-Signature"},
  "fields": {
    "node": "issue.fields.customfield_10042",
    "status": "issue.fields.status.name",
    "time": "issue.fields.updated",
    "ticket": "issue.key",
    "summary": "issue.fields.summary",
    "noticeTs": "issue.fields.created"
  },
  "statuses": {
    "In Repair": "down", "Customer Caused": "down", "Repaired": "up", "Closed": "up",
    "In Maintenance": "maintenance_start", "Maintenance Done": "maintenance_end"
  },
  "customerCaused": ["Customer Caused"]
}
```

| Field | Meaning |
| --- | --- |
| `source` | Recorded in every event; contracts with a `sources` block can admit it by this name, or by the receiver's keyring role `provider-integration` |
| `auth` | `hmacSecretFile` (at least 16 bytes; deliveries carry `hex(HMAC-SHA256(secret, body))`, optionally prefixed `sha256=`, in `hmacHeader`, default `X-Hub-Signature-256`) or `tokenSha256` (the SHA-256 of a bearer token, for systems that cannot sign), or both. Unauthenticated deliveries are always refused |
| `fields` | Dot paths into the JSON; array elements by index (`labels.0`). `node` and `status` are required. `time` defaults to the moment of delivery; RFC 3339, Jira's `2026-10-03T04:12:00.000+0000` and Unix seconds or milliseconds are all read |
| `statuses` | Ticket status → event type: `down`, `up`, `degraded_start`, `degraded_end`, `maintenance_start`, `maintenance_end`. Unlisted statuses are acknowledged and ignored |
| `customerCaused` | Statuses whose events get a cause starting with `customer:`, which contracts with `exclusions.customerCaused` exclude |
| `nodePrefix` | Stripped from node values, for systems that write `host/gpu-003` |

Map the status that is set when a window **starts**, such as "In Maintenance", not the one set when it is scheduled. The time of the announcement goes in `noticeTs` (for example when the ticket was created), and contracts exclude maintenance announced early enough.

## What gets recorded

- **Only changes.** An outage that is already open is not opened again, and a close with nothing open is not recorded, so redelivered and repeated webhooks are harmless. On start, the receiver reads its own log to learn what is open.
- **Times.** Each event is dated by the ticket's time; a time in the future becomes the moment of delivery. A delivery dated before the node's last recorded event is late or replayed, and is not recorded, so an old signed delivery sent again cannot reopen an outage. Map `time` for this protection: without it, every delivery is dated when it arrives.
- **Causes.** `ticket:OPS-4711` (the ticket id), or `ticket:<status>` without one; the status and summary go in the note.
- **Answers.** `201` recorded, `200` nothing changed (already open, nothing to close, or stale), `202` status not mapped, `400` unreadable delivery, `401` bad signature or token, `422` node not in the contract (`--contract`).

## Running it

```sh
capledger watch webhook --mapping mapping.json --contract contract.yaml \
  --log tickets.jsonl --key-dir keys/webhook --listen 127.0.0.1:8090
```

Put it behind a TLS-terminating proxy, or pass `--tls-cert` and `--tls-key`, and point the ticket system's webhook at it. `deploy/systemd/capledger-webhook.service` runs it as a service. Enrol its key without node bindings, since it speaks for every node:

```sh
capledger keyring import --keyring keyring.json --log tickets.jsonl \
  --owner "Example Neocloud" --role provider-integration --bind-nodes=false
```

To try a mapping, or to load tickets exported from the past, `--replay FILE` processes saved deliveries in order without listening (and without checking signatures, since the files are your own):

```sh
capledger watch webhook --mapping examples/webhooks/jira-repairs.json --log tickets.jsonl --key-dir keys/webhook \
  --replay examples/webhooks/01-repair-opened.json --replay examples/webhooks/02-repair-redelivered.json \
  --replay examples/webhooks/03-repair-closed.json
```

```
examples/webhooks/01-repair-opened.json: 2026-10-03T04:20:00Z gpu-003 down (…-2)
examples/webhooks/02-repair-redelivered.json: not recorded: down is already open for gpu-003
examples/webhooks/03-repair-closed.json: 2026-10-03T06:40:00Z gpu-003 up (…-3)
```

The secret in `examples/webhooks/` is for the examples only. Keep real secrets readable by the receiver alone.

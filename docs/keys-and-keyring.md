# Keys and keyrings

Every evidence log entry and every bundle is signed. A signature proves which key wrote something; it does not say who holds the key or what that holder may speak for. That is the keyring's job: an explicit, reviewable list of trusted keys, each with an owner, a role, the nodes it may report on and when it is valid.

## One key per writer

Since version 0.2 every writer has its own key by default.

| Writer | Key | Announced as |
| --- | --- | --- |
| `capledger agent` | `--key-dir`, default `/var/lib/capledger/keys` on the node | role `provider-agent`, its node |
| `capledger probe` | `--key-dir` chosen by the tenant | role `tenant-probe`, its node |
| `capledger event emit` (integrations, the Slurm watcher) | `--key-dir` or `--key` | role `provider-integration` (change with `--role`) |
| `capledger watch kubernetes` | `--key-dir` (default `/var/lib/capledger/watcher-keys`) or `--key` | role `provider-integration`, no node |
| `capledger evidence append`, `bundle create` | `--key-dir` or `--key` | no role or node |
| `capledger sla sign`, `ledger consent` (since 0.3) | a party's own key, `--key-dir` or `--key` | not announced; enrol it with `keyring add --role provider` (or `customer`, `financier`) |
| `capledger evidence checkpoint`, `ledger serve` (since 0.3) | the signer's or the ledger's witness key | not announced; enrol it with `keyring add --role witness` (or the signer's party role) |

A key directory gets a new Ed25519 key pair the first time it is used: `capledger.key.pem` (PKCS#8, mode 0600, in a directory created with mode 0700) and `capledger.pub.pem`. `--key FILE` still takes a key you manage yourself, for example one made with `capledger keygen`.

Why per node: with one shared key, anyone who copies it from any node can write convincing outages, or recoveries, for the whole cluster. With per-node keys bound to their nodes in the keyring, a stolen key can only speak for the node it came from, and anything else fails verification.

## Key announcements

The first time a writer signs a log with a key, it appends a `key` entry signed by that key:

```json
{"seq":1,"ts":"2026-10-07T09:00:00Z","kind":"key","payload":{"keyId":"655425e979f67bef","node":"gpu-003","publicKey":"-----BEGIN PUBLIC KEY-----\n…\n-----END PUBLIC KEY-----\n","role":"provider-agent","software":"capledger 0.2.0"},"prev":"0000…","keyId":"655425e979f67bef","hash":"…","sig":"…"}
```

An announcement proves that the writer holds the private key and records the node and role it claims. It does not prove who runs the writer: anyone can make a key and announce it. Trusting a key is a separate, explicit step.

## Enrolling keys

From the announcements in the logs, which is the usual way:

```bash
capledger keyring import --keyring keyring.json \
  --log evidence-gpu-001.jsonl --log evidence-gpu-002.jsonl \
  --owner "Example Neocloud" --role provider-agent
```

Each announced key is added with `nodes` set to the node it announced. Writers that speak for many nodes, such as the Slurm and Kubernetes watchers, are imported with `--bind-nodes=false`. `--key-id` limits the import to keys you name, and `--not-before` / `--not-after` set a validity window. Forged or corrupt announcements stop the import before anything is saved.

This is trust on first use. Compare the key ids with what the nodes themselves report (the agent prints its key id when it starts) before relying on them: if a node was already compromised when it first announced a key, the keyring would trust the attacker's key.

From a public key file:

```bash
capledger keyring add --keyring keyring.json --pub gpu-001.pub.pem \
  --owner "Example Neocloud" --role provider-agent --node gpu-001 --not-before 2026-10-01T00:00:00Z
capledger keyring list --keyring keyring.json
```

## What verification enforces

`--keyring` works with `evidence verify`, `events export`, `evidence gaps`, `evidence checkpoint`, `bundle verify` and `sla verify`. On top of the hash chain and the signatures, every entry must satisfy:

- the signing key is in the keyring;
- the entry's time (`ts`) is inside the key's `notBefore` / `notAfter` window (for a bundle, its creation time);
- for entries about one node (events, heartbeats, observations, key announcements), the key is bound to that node or to no node at all.

Any violation fails verification, and `events export` then exports nothing. `--pub FILE` still works and trusts that key for every node at all times, as in version 0.1. Use it for quick checks and a keyring for anything contractual; both can be given together.

Since 0.3 the keyring also decides who speaks for whom beyond single entries:

- `events export --signers` writes each event's signing key and its keyring role into the event, and a contract's `sources` can accept events by role ([SLA spec](sla-spec.md#sources-and-reconcile-rules-which-clock-wins)).
- A statement signature or a feed consent counts only if its key's role is the party it claims (`provider`, `customer`, `financier`).
- A checkpoint counts only if its signer is in the keyring and was valid when it signed.
- The [ledger](ledger-service.md) verifies every pushed entry, signature and consent against its own copy of the keyring, so both parties should keep theirs and the ledger's in step.

## File format

```json
{
  "apiVersion": "capacity-ledger/v1alpha1",
  "kind": "Keyring",
  "keys": [
    {
      "keyId": "655425e979f67bef",
      "publicKey": "-----BEGIN PUBLIC KEY-----\n…\n-----END PUBLIC KEY-----\n",
      "owner": "Example Neocloud",
      "role": "provider-agent",
      "nodes": ["gpu-003"],
      "notBefore": "2026-10-01T00:00:00Z",
      "note": "announced in evidence-gpu-003.jsonl seq 1 by capledger 0.3.0"
    }
  ]
}
```

Unknown fields are rejected, each key appears once, `keyId` must match the public key (the first 16 hex characters of its SHA-256), times are RFC 3339 and `notAfter` is exclusive. Roles are lowercase slugs; the suggested ones are `provider-agent`, `provider-integration`, `tenant-probe`, `harness` and `auditor` for evidence, and `provider`, `customer`, `financier` and `witness` for statement signatures, consents and checkpoints. Give a party's signing key its own entry: a node key that also signed statements would let anyone who steals it from one node accept a month's statement. Keep the keyring under version control next to the contract, so that both parties verify against the same file.

## Rotation and revocation

- **Rotate:** give the writer a new key directory, enrol the new key, and set `notAfter` on the old entry to the switch-over time. Keep the old entry: history signed with the old key must stay verifiable.
- **Revoke a compromised key:** set `notAfter` to the earliest time the key may have been exposed. Entries signed after that fail verification; earlier ones stay valid.
- **Re-imaged node:** a node that loses `/var/lib/capledger` creates and announces a new key. Enrol it like a new node.

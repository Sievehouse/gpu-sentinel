# Evidence formats

Capacity Ledger keeps evidence in two formats: **evidence logs**, append-only and written continuously by agents, probes and integrations, and **evidence bundles**, signed archives that freeze a set of files such as an acceptance report and its raw results.

## Evidence logs

An evidence log is a JSON Lines file. Each line is one entry:

```json
{"seq":2,"ts":"2026-10-21T21:43:10.123Z","kind":"event","payload":{"cause":"xid:79","id":"7acb4797ce3869f0-2","node":"gpu-003","seq":2,"source":"capledger-agent","ts":"2026-10-03T04:11:58Z","type":"down"},"prev":"4f3b…","keyId":"7acb4797ce3869f0","hash":"c08e…","sig":"base64…"}
```

| Field | Meaning |
| --- | --- |
| `seq` | 1, 2, 3 … with no gaps |
| `ts` | When the entry was written (RFC 3339, UTC); event times live in the payload |
| `kind` | What the payload is; see below |
| `payload` | Canonical JSON of the recorded object |
| `prev` | `hash` of the previous entry; 64 zeros for the first |
| `keyId` | First 16 hex characters of SHA-256 over the Ed25519 public key |
| `hash` | SHA-256 of `"capacity-ledger/evidence/v1\n"` followed by the canonical JSON of `{seq, ts, kind, payload, prev, keyId}` |
| `sig` | Base64 Ed25519 signature over the 32 raw bytes of `hash` |

### Entry kinds

| Kind | Written by | Payload |
| --- | --- | --- |
| `key` | every writer, once per key and log | Key announcement: `keyId`, `publicKey` (PEM), and the claimed `node`, `role` and `software`. Signed by the key it announces ([keys and keyrings](keys-and-keyring.md)). |
| `heartbeat` | agent, probe | `{"node":"gpu-003","ts":"2026-10-07T09:01:00Z","intervalSeconds":60,"role":"agent","gpus":8}`: alive at `ts`, next heartbeat promised within `intervalSeconds`. `gpus` is the last GPU count, when known. |
| `observation` | agent, probe | A raw fault or measurement: the parsed kernel line (with the line itself), the policy action and version, or a GPU count or benchmark result |
| `event` | agent, probe, integrations | An availability event for the SLA engine ([SLA spec](sla-spec.md#events)); `evidence` points at the observation behind it |
| `statement` | `evidence append` | A sealed SLA statement |
| `bundle` | `bundle create --log` | `bundleId`, `bundleType`, `subject`, `created`, `files`, `totalBytes`, `signerKeyId`: commits the log to a bundle's exact contents |
| anything else | `evidence append` | For example `acceptance-report` or `test-result` |

### Canonical JSON

Object keys sorted, no insignificant whitespace, no HTML escaping, numbers exactly as written. Payloads avoid floating point where exactness matters; money and percentages are decimal strings.

### Verification

`capledger evidence verify --log L --keyring K` (or `--pub P`) checks every entry: the sequence has no gaps, `prev` matches the previous entry's hash, the stored hash matches the content, and the signature is valid for a trusted key. With a keyring it also enforces each key's validity window and node bindings. It reports every problem it finds, plus the Merkle root over all entry hashes.

Detected: edited payloads or fields, deleted or reordered entries, entries signed by unknown keys, keys used outside their validity window or for nodes they are not bound to, truncated chains that are then extended.
Not detected by the log alone: deleting the *last* entries (compare the last hash or Merkle root with a copy held by the other party), stopping the writer (look for gaps with `capledger evidence gaps`), or a writer who signs false observations with a valid key (see the [threat model](threat-model.md)).

`events export` and `evidence gaps` verify the very bytes they then read, so a log that grows between verification and export cannot slip in unverified entries.

### Merkle root

Leaves are the raw entry hashes. The tree follows RFC 6962: leaf hash = SHA-256(0x00 ‖ leaf), node hash = SHA-256(0x01 ‖ left ‖ right), split at the largest power of two below n. Publishing the root to the counterparty each day (or embedding it in a statement) pins the whole history.

## Evidence bundles

`capledger bundle create` packs files into a gzip-compressed tar archive:

```text
manifest.json          always the first entry; lists and signs everything else
files/report.json
files/report.md
files/raw/gpu-001/dcgm-diag.txt
…
```

```json
{
  "apiVersion": "capacity-ledger/v1alpha1",
  "kind": "EvidenceBundle",
  "bundleType": "acceptance",
  "subject": "example-hgx-32gpu",
  "created": "2026-10-07T12:00:00Z",
  "note": "optional free text",
  "files": [
    {"path": "raw/gpu-001/dcgm-diag.txt", "sha256": "…", "size": 52},
    {"path": "report.json", "sha256": "…", "size": 1874}
  ],
  "signer": {"keyId": "655425e979f67bef", "publicKey": "-----BEGIN PUBLIC KEY-----\n…"},
  "signature": "base64…"
}
```

- `signature` is an Ed25519 signature over SHA-256 of `"capacity-ledger/bundle/v1\n"` followed by the canonical JSON of the manifest with `signature` set to the empty string.
- The **bundle id** is the SHA-256 of the canonical JSON of the complete, signed manifest. `--log` records it in an evidence log as a `bundle` entry.
- Suggested types: `acceptance` (report, plan, results, inventory), `statement` (a statement with its contract, events and evidence logs), `incident`. Any lowercase slug is accepted.

`capledger bundle verify --bundle B --keyring K` (or `--pub P`) checks, in this order:

1. `manifest.json` is the first entry, is well formed, and lists 1 to 10,000 files sorted by path;
2. the signer is trusted, valid at the bundle's `created` time, and its embedded public key matches the trusted one;
3. the signature is valid; nothing else is read unless it is;
4. every other entry is a regular file under `files/`, listed in the manifest exactly once, with the listed size and SHA-256;
5. every listed file is present.

Links, devices, directories, extra or duplicated files and files that differ from the manifest all fail verification. Files are streamed through SHA-256, so memory use does not grow with the bundle. `--extract DIR` writes the files into a new or empty directory and removes everything again if verification fails.

Paths inside a bundle are restricted so that every bundle extracts the same way on Linux, macOS and Windows: parts separated by `/`, each starting with a letter, digit or underscore and using only letters, digits, spaces and `._+@,~-`, not ending in a dot or space, and not a Windows device name such as `con` or `nul.txt`. Two paths may not differ only in case, and a path cannot be both a file and a directory.

The signature covers the manifest, not the compressed bytes, so recompressing a bundle keeps it valid. One build of the tool always produces the same bytes from the same files, time (`--created`) and key.

## Keys

`capledger keygen` writes `capledger.key.pem` (PKCS#8, mode 0600) and `capledger.pub.pem` (PKIX). Writers given `--key-dir` create such a pair on first use. Share public keys with counterparties, never commit private keys, and see [keys and keyrings](keys-and-keyring.md) for enrolment, node bindings, rotation and revocation.

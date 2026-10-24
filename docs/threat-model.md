# Threat model

Capacity Ledger sits between parties with opposing financial interests: a provider who owes credits, a customer who receives them, and financiers who price risk on the result. The design assumes each party may try to shade the record in its favour, and that the agent itself is a new piece of software on high-value hosts.

## Assets

1. The integrity and completeness of evidence logs and bundles.
2. Signing keys and the keyring that says which keys to trust.
3. The correctness and replayability of statements.
4. The GPU hosts the agent runs on.

## Threats and mitigations

| Threat | Mitigation in v0.2 | Residual risk and plan |
| --- | --- | --- |
| A party edits, deletes or reorders past entries | Hash chain, signatures, sequence numbers; `evidence verify` reports every break | Truncating the newest entries is invisible without an external copy. **Plan:** exchange daily Merkle roots between parties; hosted ledger with co-signing |
| The provider stops the agent, or filters its input, during an outage | Heartbeats every minute; `events export --derive-gaps` turns silences into unmonitored time, which every statement lists and which a contract can count as downtime (`unmonitored: count-as-down`) | Filtering individual kernel lines while heartbeats continue is not detectable by the agent itself; the tenant probe is the independent check |
| A provider-controlled agent records false data | Open-source agent; every observation stores the raw log line; policy version recorded; the tenant probe gives the customer its own record, and `sla corroborate` shows every stretch where the two disagree | A root-level adversary on the host can lie to both. **Plan:** GPU attestation and TPM host attestation where available |
| The customer injects false outages | Events carry their source and signing key; the provider's statement uses the provider's logs; tenant evidence is compared, not merged | **Plan:** contract-level allow-lists of sources and keys |
| A signing key is stolen from one node | Per-node keys by default; a keyring binds each key to its node, so the stolen key cannot speak for other nodes; validity windows revoke it from a chosen time | The stolen key can still lie about its own node until revoked. **Plan:** optional hardware-backed keys |
| A forged key is enrolled | Announcements prove possession only; enrolment is an explicit `keyring import` or `keyring add` with an owner and role; forged announcements stop the import | Trust on first use: a node compromised before enrolment can enrol an attacker's key. Compare key ids out of band |
| An acceptance report is altered after hand-over | Reports and raw results travel as a signed bundle; any changed, added or removed file fails verification; the bundle id can be recorded in an evidence log | The bundle proves who signed and what, not that the tests were run honestly; keep the raw tool output in the bundle |
| A malicious bundle attacks the verifier | Manifest first and verified before anything else is read; streaming hashes; size and count limits; path rules that rule out traversal, links and case collisions; extraction only into an empty directory, removed again on failure | Decompression still costs CPU on a hostile input |
| Ambiguous contract terms exploited later | Spec rejects unknown fields and non-decimal numbers; the YAML reader rejects anchors, tags and other constructs that hide meaning; statements fingerprint the exact contract | Free-text contract clauses remain outside the engine |
| Replays with a different engine give different results | Statements record the semantics they need; old statements keep their hashes; unknown semantics are refused | Engine changes require new semantics and a migration note |
| The agent becomes an attack path into GPU hosts | Read-only, no network listener, no third-party code, runs as non-root with all capabilities dropped (kern.log mode); the init container that prepares its directory only changes ownership | /dev/kmsg mode needs a privileged container; prefer the log-file mode |
| Sensitive data leaks through evidence | Only device counters, kernel fault lines and test results are recorded; no job contents, datasets or weights | Kernel lines can include process names; document and offer redaction |

## Out of scope for v0.2

Network transport of logs and bundles (they are files; ship them with your existing tooling), multi-tenant hosting, and access control, all of which belong to the planned ledger service.

# Threat model

GPU Sentinel sits between parties with opposing financial interests: a provider who owes credits, a customer who receives them, and financiers who price risk on the result. The design assumes each party may try to shade the record in its favour, that the agent itself is a new piece of software on high-value hosts, and (since 0.3) that whoever runs the ledger may be careless or compromised.

## Assets

1. The integrity and completeness of evidence logs and bundles.
2. Signing keys and the keyring that says which keys to trust and for what.
3. The correctness and replayability of statements, and the parties' signatures on them.
4. The GPU hosts the agent runs on.
5. The ledger's data, its witness key and the parties' bearer tokens.

## Threats and mitigations

| Threat | Mitigation in v0.3 | Residual risk and plan |
| --- | --- | --- |
| A party edits, deletes or reorders past entries | Hash chain, signatures, sequence numbers; `evidence verify` reports every break | None for entries before a checkpoint the other party holds; see the next row for the newest entries |
| A writer truncates its newest entries, or rewrites its whole history and re-signs it with its own valid key | Checkpoints: the ledger's witness signs every log's size, last hash and Merkle root daily, parties can sign and exchange their own, and `evidence verify --checkpoint` fails a log that no longer extends a checkpoint | Entries written after the last checkpoint the other party holds can still be cut. Checkpoint often; push continuously |
| The provider stops the agent, or filters its input, during an outage | Heartbeats every minute; `events export --derive-gaps` turns silences into unmonitored time, which every statement lists and which a contract can count as downtime (`unmonitored: count-as-down`) | Filtering individual kernel lines while heartbeats continue is not detectable by the agent itself; the tenant probe is the independent check |
| A provider-controlled agent records false data | Open-source agent; every observation stores the raw log line; policy version recorded; the tenant probe gives the customer its own record; under reconcile rules the customer's record counts too, and `sla corroborate` shows every stretch where the two disagree | A root-level adversary on the host can lie to both. **Plan:** GPU attestation and TPM host attestation where available |
| The customer injects false outages | Events carry their signer and source; a contract's `sources` admits only listed roles or names, and an event with a signer role is classified by that role only, so a key cannot pass its events off as the other party's; reconcile rules (`count: both`) can require both records | Under `count: either` a customer key can claim outages the provider did not see; that is the rule the parties chose, and corroboration lists every such stretch |
| A signing key is stolen from one node | Per-node keys by default; a keyring binds each key to its node, so the stolen key cannot speak for other nodes; validity windows revoke it from a chosen time | The stolen key can still lie about its own node until revoked. **Plan:** optional hardware-backed keys |
| A party's statement or consent key is stolen | Party keys are separate keyring entries with roles `provider`, `customer`, `financier`; validity windows revoke them; a party's latest signature counts, so a forged acceptance can be overridden by a new signed dispute | Keep party keys off GPU nodes, offline or in a hardware token where possible |
| One party signs a statement as the other | The keyring role of the signing key must equal the claimed party; the ledger also requires the uploading token's party to match | Both parties must agree on the keyring; keep it under version control next to the contract |
| A forged key is enrolled | Announcements prove possession only; enrolment is an explicit `keyring import` or `keyring add` with an owner and role; forged announcements stop the import | Trust on first use: a node compromised before enrolment can enrol an attacker's key. Compare key ids out of band |
| The ledger operator, or an attacker on the ledger host, alters data | The ledger computes nothing; logs, statements, signatures, consents and checkpoints are signed by keys it does not hold (except its witness key), so altering them breaks their signatures; parties pull logs and verify them against their own keyring and their own checkpoints | The ledger could withhold data or refuse pushes; parties keep their own copies and checkpoints. A compromised witness key can sign false checkpoints going forward: rotate it with `notAfter`, and exchange party-signed checkpoints as a second line |
| A bearer token is stolen | Tokens are stored only as SHA-256; each token can do only what its party's role allows; a stolen token cannot forge entries, signatures or consents without the matching private keys, and cannot write to another party's logs | A stolen token can push validly signed entries of that party or read what that party may read. Rotate it by changing the hash in the configuration and sending SIGHUP |
| A financier sees more than agreed | The feed opens only while both the provider's and the customer's latest signed consents are grants; it holds statements, signatures and checkpoints, never raw logs; either party can revoke at any time | Whatever a financier already downloaded stays with it |
| Resource exhaustion against the ledger | Request bodies are capped (64 MiB for pushes, 16 MiB for documents); header read timeout; one writer per data directory | No rate limiting; put the ledger behind a proxy that rate-limits if it faces the internet |
| An acceptance report is altered after hand-over | Reports and raw results travel as a signed bundle; any changed, added or removed file fails verification; the bundle id can be recorded in an evidence log | The bundle proves who signed and what, not that the tests were run honestly; keep the raw tool output in the bundle |
| A malicious bundle attacks the verifier | Manifest first and verified before anything else is read; streaming hashes; size and count limits; path rules that rule out traversal, links and case collisions; extraction only into an empty directory, removed again on failure | Decompression still costs CPU on a hostile input |
| Ambiguous contract terms exploited later | Spec rejects unknown fields and non-decimal numbers; the YAML reader rejects anchors, tags and other constructs that hide meaning; reconcile rules must be explicit (no defaults); statements fingerprint the exact contract | Free-text contract clauses remain outside the engine |
| Replays with a different engine give different results | Statements record the semantics they need; old statements keep their hashes; unknown semantics are refused; an independent Python implementation is compared with the engine in CI | Engine changes require new semantics and a migration note |
| The agent becomes an attack path into GPU hosts | Read-only, no network listener, no third-party code, runs as non-root with all capabilities dropped (kern.log mode); the init container that prepares its directory only changes ownership | /dev/kmsg mode needs a privileged container; prefer the log-file mode |
| Fault injection reaches production | `inject_fault.sh` refuses to run without `--yes`, records every injection, keeps undo commands, and is documented for rented test clusters only | Operators can still point it at the wrong host |
| Sensitive data leaks through evidence | Only device counters, kernel fault lines and test results are recorded; no job contents, datasets or weights | Kernel lines can include process names; document and offer redaction |

## Out of scope for v0.3

Hosting many contracts on one ledger, a database backend, rate limiting and single sign-on for the ledger, and attestation of the hosts that run agents and probes.

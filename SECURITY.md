# Security policy

## Reporting a vulnerability

Please do not open public issues for security problems. Email **ymohit799057@gmail.com** with a description, affected versions and, if possible, a proof of concept. You will get an acknowledgement within three working days.

## In scope

- The `capledger` agent, the tenant probe and anything else that runs on GPU hosts.
- Evidence-log integrity: ways to alter, remove or forge entries without `evidence verify` noticing (other than the limits documented in `docs/evidence-format.md`).
- Keyring enforcement: entries or bundles accepted for a node or a time that their key is not enrolled for.
- Evidence bundles: ways to make `bundle verify` accept changed, added or removed files, or to make `--extract` write outside its directory.
- SLA engine determinism: inputs that produce different statements on different machines.
- Key handling.

## Supported versions

Only the latest release receives fixes during the pre-1.0 period.

See `docs/threat-model.md` for the assumptions behind the design.

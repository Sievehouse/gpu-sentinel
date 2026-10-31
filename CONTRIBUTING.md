# Contributing

Thanks for helping. This project produces evidence that can move money between companies, so correctness and reviewability matter more than speed.

## Development setup

```bash
make build                     # Go 1.22+
python3 -m venv .venv && . .venv/bin/activate
pip install -e "harness[dev]"  # Python 3.10+
make test                      # gofmt check, go vet, Go tests, Python tests
./scripts/demo.sh              # also starts and stops a local ledger
                               # on Windows: powershell -File scripts\demo.ps1 (keep both in step)
cd harness && CAPLEDGER_BIN=../bin/capledger pytest -q   # include the engine vs reference comparison
```

## Ground rules

- **No new dependencies in Go code** without discussion. The agent, engine and ledger are standard-library only so that counterparties can audit everything they run.
- **Every change to the SLA engine** needs a test that pins the new behaviour, an updated worked example in `docs/sla-spec.md` if numbers change, and an `EngineVersion` bump, and the same change in the Python reference implementation (`harness/src/capledger_harness/sla_reference.py`), written from the spec, so that the two keep agreeing on random months. Statements that do not use a new feature must stay byte-identical (the version 0.1 and 0.3 hashes are pinned in `internal/sla/engine_test.go` and `reconcile_test.go`), and statements that do must record the new semantics, so that older binaries refuse them instead of disagreeing.
- **No floating point** in anything that becomes availability, credits or money.
- **Fault policy changes** must cite the vendor documentation they rely on in the pull request (NVIDIA's XID catalogue; the amdgpu kernel and ROCm documentation for AMD).
- **Wire formats** (evidence entries, keyrings, bundles, contracts, checkpoints, signatures, consents, the ledger API) only gain optional fields; anything else needs a new `apiVersion` or domain string.
- **The ledger computes nothing.** Anything it serves must stay verifiable offline with the ordinary commands; it must never need to be trusted for a number.
- **Fault injection** (`harness/scripts/inject_fault.sh`) stays behind `--yes`, records every change and can undo it.
- Keep the agent read-only. Features that change host state belong elsewhere.

## Style

Go: `gofmt`, `go vet`, small packages under `internal/`. Python: type hints, standard library where possible, tests next to behaviour in `harness/tests/`.

## Pull requests

Describe the behaviour change, the evidence it is right, and how you tested it. Synthetic data is fine for tests; never commit real customer logs, contracts or keys.

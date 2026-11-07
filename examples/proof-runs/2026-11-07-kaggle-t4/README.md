# First real-hardware run: Kaggle, 2 x NVIDIA T4

The first run of the agent and the proof-of-value harness outside this
repository's own development environment. Kaggle notebooks don't give root
or device access, so this covers the parts of the proof that don't need it:
detection from a kernel line and passive overhead. It does not cover the
root-only fault injections (F1-F5 in `docs/roadmap.md`), which still need a
rented or owned cluster.

Environment: a Kaggle notebook, GPU accelerator "T4 x2", Go 1.27.3 installed
fresh, `gpu-sentinel` cloned fresh from this repository's `main` branch and
built from source. capledger 0.5.0, sla engine 0.3.0.

## What was run

A synthetic NVIDIA XID 79 line ("GPU has fallen off the bus") was appended to
a plain log file the agent was watching with `--source` (not `/dev/kmsg`,
which a Kaggle container cannot write to):

```sh
./capledger agent --source kern.log --log events.jsonl --key-dir keys &
echo '<3>NVRM: Xid (PCI:0000:00:04): 79, GPU has fallen off the bus' >> kern.log
```

[`events.jsonl`](events.jsonl) is the resulting log, unedited. The agent
recorded a signed `down` event (seq 4) within about two seconds, with a
`xid:79` cause, preceded by its own signing key record and heartbeats.
[`agent.log`](agent.log) is that agent's own console output (`created
signing key ... / observed xid 79 ... / event ... down ...`), and
[`signing-key.pub.pem`](signing-key.pub.pem) is the public half of the key it
signed with, verbatim from the session (it matches the `publicKey` field
embedded in `events.jsonl`'s first entry).

A second agent instance was started against `kern2.log` for the overhead
test below; [`events2.jsonl`](events2.jsonl) and [`agent2.log`](agent2.log)
are its unedited log and console output — seven entries, all heartbeats,
since no fault was fed to this instance.

## Results

- **Signed chain verifies**: `capledger evidence verify --log events.jsonl --pub signing-key.pub.pem --format text` printed:

  ```
  events.jsonl  OK  6 entries, keys [8fdc64d260fd9c46], merkle root 5b986fa83d775f7e8f9c0ec93499ebd3252e6e5deea8e890c0ffb0b2741befde
  ```

- **Agent overhead**, measured with `harness/scripts/agent_overhead.py` for 30 seconds while a real GEMM workload ran on both GPUs: [`overhead.json`](overhead.json). 0.067% of one CPU core and 11.4 MB resident, against the roadmap's thresholds of 1% and 200 MB — roughly 15x and 17x under budget.

- **`harness/scripts/gemm_bench.py`**, whose own docstring says "not yet validated on hardware in this repository," ran on both GPUs:

  ```
  gpu 0: 2.4 TFLOPS
  gpu 1: 2.4 TFLOPS
  ```

## What this does not show

No hardware fault was actually injected (`inject_fault.sh`'s `power-cap`,
`lock-clocks`, `link-down` and `io-throttle` all need root or device access a
Kaggle container does not grant); only the input to the agent was simulated.
Recall, false-flag rate and localisation (the other proof-of-value
thresholds) are untested here and still need a rented or owned cluster per
`docs/proof-of-value.md`.

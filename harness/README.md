# capledger-harness

The Python half of Capacity Ledger: plans and runs cluster acceptance tests with standard tools, parses their output, compares nodes with their peers and localises faulty nodes.

```bash
pip install -e "harness[dev]"

capledger-harness plan --inventory ../examples/inventory/hgx-4-node.yaml --out plan.json
capledger-harness run --plan plan.json --out results.jsonl --dry-run
capledger-harness report --results results.jsonl --cluster example-hgx-32gpu --expected-gpus 8 \
  --criteria ../examples/criteria/hgx-h100-400g.yaml --metrics ../examples/samples/node_metrics.json \
  --out report.json --markdown report.md
capledger-harness outliers --metrics ../examples/samples/node_metrics.json
capledger-harness localize-sim --nodes 64 --bad 2
capledger-harness parse-nccl ../examples/samples/nccl_all_reduce_4node.txt
```

`convert-contract` turns a YAML contract into JSON. Since version 0.2 the `capledger` engine reads YAML directly, so you only need it for other tools.

## AMD Instinct clusters

Set `vendor: amd` in the inventory (see `../examples/inventory/mi300x-2-node.yaml`). The audit then counts the PCI functions bound to the `amdgpu` driver and records `amd-smi` output, the single-node stage runs the ROCm Validation Suite GPU stress test (or your own `single_node_command`, which must exit non-zero on failure), and the collective stage runs rccl-tests, whose binaries and output match nccl-tests. AMD support is reference code that has not yet been validated on hardware in this repository.

## Sign the results

Pack the report and its raw inputs into one signed bundle, and record the bundle in the evidence log, with the Go CLI:

```bash
capledger bundle create --out acceptance.tar.gz --type acceptance --subject example-hgx-32gpu \
  --key-dir keys/harness --file report.json --file report.md --file results.jsonl --file plan.json \
  --file inventory.yaml=../examples/inventory/hgx-4-node.yaml --log evidence.jsonl
capledger bundle verify --bundle acceptance.tar.gz --pub keys/harness/capledger.pub.pem
```

Anyone holding the public key can check the bundle offline; changing, adding or removing a single file makes verification fail.

`scripts/burnin.py` needs PyTorch on GPU nodes (`pip install -e "harness[burnin]"`). It is reference code and has not yet been validated on hardware in this repository.

Tests: `pytest -q` (no GPUs needed; tool output is simulated).

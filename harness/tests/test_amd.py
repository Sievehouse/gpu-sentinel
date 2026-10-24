import json

import pytest

from capledger_harness.inventory import Inventory
from capledger_harness.parsers import count_amd_gpus, count_gpus, rvs_failed
from capledger_harness.plan import Plan, build_plan
from capledger_harness.report import Criteria, build_report
from capledger_harness.runner import Completed, load_results, run_plan

PCI = ["0000:05:00.0", "0000:15:00.0", "0000:65:00.0", "0000:75:00.0", "0000:85:00.0", "0000:95:00.0", "0000:e5:00.0", "0000:f5:00.0"]


def audit_output(addresses):
    # amd-smi prints its own "GPU: n" and BDF lines in a later section; they must not be counted.
    amd_smi = "".join(f"GPU: {i}\n    BDF: {a}\n    UUID: x\n" for i, a in enumerate(PCI))
    return "\n".join(addresses) + "\n--- amd-smi\n" + amd_smi + "--- kernel\n6.8.0-45-generic\n"


def test_amd_inventory_and_plan(examples):
    inv = Inventory.load(examples / "inventory" / "mi300x-2-node.yaml")
    assert inv.vendor == "amd"
    plan = build_plan(inv)
    audit = plan.stage("audit")[0]
    assert "/sys/bus/pci/drivers/amdgpu" in audit.command and audit.meta == {"vendor": "amd"}
    diag = plan.stage("single_node")
    assert [t.id for t in diag] == ["rvs:mi300-01", "rvs:mi300-02"]
    assert "gst_single.conf" in diag[0].command
    assert "/opt/rccl-tests/build/all_reduce_perf" in plan.stage("collectives")[0].command
    again = Plan.from_dict(json.loads(json.dumps(plan.to_dict())))
    assert again.vendor == "amd" and [t.id for t in again.tasks] == [t.id for t in plan.tasks]


def test_single_node_command_and_validation():
    base = {"cluster": "x", "launcher": "slurm", "nodes": ["a"], "gpus_per_node": 8, "vendor": "amd"}
    plan = build_plan(Inventory.from_dict(base | {"single_node_command": "/opt/amd/agfhc/agfhc -r all_lvl5"}))
    assert [t.id for t in plan.stage("single_node")] == ["diag:a"]
    with pytest.raises(ValueError):
        Inventory.from_dict(base | {"vendor": "intel"})
    with pytest.raises(ValueError):
        Inventory.from_dict(base | {"single_node_command": " "})
    # Version 0.1 plans have no vendor and still load.
    assert Plan.from_dict({"cluster": "x", "gpusPerNode": 8, "nodes": ["a"], "tasks": []}).vendor == "nvidia"


def test_parsers():
    assert count_amd_gpus(audit_output(PCI)) == 8
    assert count_amd_gpus(audit_output(PCI[:7])) == 7
    assert count_gpus("0, a\n1, b\n--- topo\n2, c\n") == 2
    assert rvs_failed("[RESULT] [1033.2] [gst-1] [GPU:: 28851] GFLOPS 9100.1 Target GFLOPS: 10000 met: FALSE\n")
    assert rvs_failed("[RESULT] [12.3] [gpup-1] pass: false")
    assert not rvs_failed("[RESULT] [1033.2] [gst-1] [GPU:: 28851] GFLOPS 10913.6 Target GFLOPS: 10000 met: TRUE\n")


def test_amd_report(tmp_path, examples):
    inv = Inventory.load(examples / "inventory" / "mi300x-2-node.yaml")

    def run(task):
        node = task.nodes[0]
        if task.kind == "audit":
            return Completed(0, audit_output(PCI if node == "mi300-01" else PCI[:7]), "")
        if task.kind == "rvs":
            met = "FALSE" if node == "mi300-01" else "TRUE"
            return Completed(0, f"[RESULT] [1.0] [gst-1] [GPU:: 1] GFLOPS 9100.1 Target GFLOPS: 10000 met: {met}\n", "")
        return Completed(0, "ok", "")

    out = tmp_path / "results.jsonl"
    run_plan(build_plan(inv), out, stages=["audit", "single_node"], executor=run)
    rep = build_report(load_results(out), Criteria(), cluster=inv.cluster, expected_gpus=8)
    reasons = {row["node"]: row["reasons"] for row in rep["nodes"]}
    assert reasons["mi300-01"] == ["ROCm Validation Suite reported a failed check"]
    assert reasons["mi300-02"] == ["only 7 of 8 GPUs visible"]
    assert rep["summary"]["reject"] == 2

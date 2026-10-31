import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

import pytest

from capledger_harness.cli import main
from capledger_harness.pov import load_trials, plan_trials, score

SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "inject_fault.sh"


def report(path: Path, verdicts: dict) -> str:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps({"nodes": [{"node": n, "verdict": v, "reasons": []} for n, v in verdicts.items()]}))
    return str(path.name)


def write_trials(tmp_path: Path, rows: list) -> str:
    p = tmp_path / "trials.jsonl"
    p.write_text("\n".join(json.dumps(r) for r in rows) + "\n")
    return str(p)


NODES = [f"gpu-{i:03d}" for i in range(1, 11)]


def test_score_meets_thresholds(tmp_path):
    rows = []
    for r in range(10):  # ten clean runs, one false flag: 1 per 100 node-tests
        flagged = ["gpu-001"] if r == 0 else []
        rows.append({"trial": f"f0-r{r}", "fault": "F0", "flagged": flagged, "nodes": NODES})
    for mag in (10, 20):
        rows.append({"trial": f"f1-{mag}", "fault": "F1", "magnitude": mag, "injected": ["gpu-004"],
                     "report": report(tmp_path / f"f1-{mag}.json", {n: ("reject" if n == "gpu-004" else "accept") for n in NODES}), "rounds": 3})
    rows.append({"trial": "f1-5", "fault": "F1", "magnitude": 5, "injected": ["gpu-005"],
                 "report": report(tmp_path / "f1-5.json", {n: ("review" if n == "gpu-005" else "accept") for n in NODES})})
    rows.append({"trial": "f3", "fault": "F3", "injected": ["gpu-002"], "flagged": ["gpu-002"], "nodes": NODES, "rounds": 2})
    s = score(load_trials(write_trials(tmp_path, rows)))
    assert s["verdict"] == "pass", s
    # One false flag in 136 clean node-tests: 100 from the clean runs, 9 from each faulty run.
    assert s["checks"]["false_flags_per_100_node_tests"]["value"] == round(100 / 136, 4)
    assert s["recall_by_fault"]["F1"] == {"detected": 3, "injected": 3}
    # Counting only rejects, the 5% review no longer counts as detected.
    strict = score(load_trials(write_trials(tmp_path, rows), ("reject",)))
    assert strict["checks"]["recall_at_5_percent"]["result"] == "fail"
    assert strict["verdict"] == "fail"


def test_score_reports_missing_data_and_misses(tmp_path):
    rows = [
        {"trial": "f1-10", "fault": "F1", "magnitude": 10, "injected": ["gpu-003"], "flagged": ["gpu-007"], "nodes": NODES, "rounds": 4},
    ]
    s = score(load_trials(write_trials(tmp_path, rows)))
    assert s["checks"]["recall_at_10_percent"]["result"] == "fail"
    assert s["checks"]["recall_at_5_percent"]["result"] == "no data"
    assert s["checks"]["max_localisation_rounds"]["result"] == "fail"
    assert s["misses"][0]["node"] == "gpu-003" and s["false_flags"][0]["node"] == "gpu-007"


@pytest.mark.parametrize("row, msg", [
    ({"trial": "x", "fault": "F9", "flagged": [], "nodes": NODES}, "unknown fault"),
    ({"trial": "x", "fault": "F0", "injected": ["gpu-001"], "flagged": [], "nodes": NODES}, "injects nothing"),
    ({"trial": "x", "fault": "F1", "flagged": [], "nodes": NODES}, "name the injected"),
    ({"trial": "x", "fault": "F1", "injected": ["gpu-099"], "flagged": [], "nodes": NODES}, "were not tested"),
    ({"trial": "x", "fault": "F1", "injected": ["gpu-001"]}, "give 'report'"),
])
def test_bad_trials(tmp_path, row, msg):
    with pytest.raises(ValueError, match=msg):
        load_trials(write_trials(tmp_path, [row]))


def test_plan_matrix_is_deterministic():
    a, b = plan_trials(NODES, 2, 5), plan_trials(NODES, 2, 5)
    assert [t.injected for t in a] == [t.injected for t in b]
    faults = [t.fault for t in a]
    assert faults.count("F0") == 20 and faults.count("F1") == 6
    f1 = next(t for t in a if t.fault == "F1")
    assert f1.inject[0].endswith(f"power-cap {f1.magnitude:g}") and f1.restore[0].endswith("restore")
    assert all(not t.restore for t in a if t.fault == "F6")


def test_cli_plan_and_score(tmp_path, examples, capsys):
    out = tmp_path / "trials.jsonl"
    assert main(["pov-plan", "--inventory", str(examples / "inventory" / "hgx-4-node.yaml"), "--repeats", "1", "--out", str(out)]) == 0
    rows = [json.loads(l) for l in out.read_text().splitlines() if l.startswith("{")]
    assert len(rows) == 18 and rows[0]["fault"] == "F0" and rows[0]["report"] == "runs/f0-r1/report.json"
    clean = tmp_path / "clean.jsonl"
    clean.write_text(json.dumps({"trial": "f0", "fault": "F0", "flagged": [], "nodes": NODES}) + "\n")
    assert main(["pov-score", "--trials", str(clean), "--out", str(tmp_path / "s.json")]) == 0
    assert "verdict: INCOMPLETE" in capsys.readouterr().out


needs_bash = pytest.mark.skipif(sys.platform == "win32" or not shutil.which("bash") or not shutil.which("tac"), reason="needs bash")


@needs_bash
def test_inject_fault_script(tmp_path):
    fake = tmp_path / "bin"
    fake.mkdir()
    calls = tmp_path / "calls.log"
    for tool in ("nvidia-smi", "ip"):
        (fake / tool).write_text(f"""#!/bin/sh
echo "{tool} $*" >> "{calls}"
case "$*" in
  *--query-gpu=index*) printf '0\\n1\\n' ;;
  *power.default_limit*) echo 700.00 ;;
esac
""")
        (fake / tool).chmod(0o755)
    cg = tmp_path / "cgroup" / "job"
    cg.mkdir(parents=True)
    (cg / "io.max").write_text("")
    env = dict(os.environ, PATH=f"{fake}:{os.environ['PATH']}", CAPLEDGER_FAULT_STATE=str(tmp_path / "state"),
               CAPLEDGER_NCCL_CONF=str(tmp_path / "nccl.conf"), CAPLEDGER_KMSG=str(tmp_path / "kmsg"),
               CAPLEDGER_CGROUP_ROOT=str(tmp_path / "cgroup"))
    truth = tmp_path / "truth.jsonl"

    def run(*args, ok=True):
        p = subprocess.run(["bash", str(SCRIPT), "--truth", str(truth), "--node", "gpu-007", *args],
                           env=env, capture_output=True, text=True)
        assert (p.returncode == 0) == ok, p.stdout + p.stderr
        return p.stdout + p.stderr

    assert "pass --yes" in run("power-cap", "10", ok=False)
    run("--yes", "power-cap", "10")
    run("--yes", "nccl-misconfig", "NCCL_IB_GID_INDEX=7")
    run("--yes", "io-throttle", "job", "259:0", "1000000")
    run("--yes", "xid-drill", "79", "0000:3b:00")
    assert "unexpected characters" in run("--yes", "nccl-misconfig", 'NCCL_X="1"', ok=False)
    assert "F1\tgpu0:-10%" in run("status")
    assert (tmp_path / "nccl.conf").read_text() == "NCCL_IB_GID_INDEX=7\n"
    assert (cg / "io.max").read_text() == "259:0 rbps=1000000 wbps=1000000\n"
    assert "NVRM: Xid (PCI:0000:3b:00): 79," in (tmp_path / "kmsg").read_text()
    assert "nvidia-smi -i 1 -pl 630" in calls.read_text()

    run("restore")
    assert not (tmp_path / "nccl.conf").exists()
    assert (cg / "io.max").read_text() == "259:0 rbps=max wbps=max\n"
    assert "nvidia-smi -i 0 -pl 700.00" in calls.read_text()
    assert "no faults applied" in run("status")
    lines = [json.loads(l) for l in truth.read_text().splitlines()]
    assert [l["action"] for l in lines].count("inject") == 4 and [l["action"] for l in lines].count("restore") == 4
    assert all(l["node"] == "gpu-007" for l in lines)
    # --dry-run changes nothing.
    assert "would run: nvidia-smi -i 0 -pl 630" in run("--dry-run", "power-cap", "10")
    assert "no faults applied" in run("status")


@pytest.mark.skipif(not os.path.exists("/proc/self/stat"), reason="needs Linux /proc")
def test_agent_overhead_measures_a_process():
    script = Path(__file__).resolve().parents[1] / "scripts" / "agent_overhead.py"
    child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(5)"])
    try:
        p = subprocess.run([sys.executable, str(script), "--pid", str(child.pid), "--seconds", "0.5", "--interval", "0.1"],
                           capture_output=True, text=True)
    finally:
        child.kill()
        child.wait()
    res = json.loads(p.stdout)
    assert p.returncode == 0 and res["pass"] and res["peakRssMB"] > 0 and res["cpuPercentOfOneCore"] < 1.0

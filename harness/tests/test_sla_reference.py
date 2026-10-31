"""The Python reference against the documented examples and, when a
capledger binary is available, against the Go engine on random months."""

import json
import os
import random
import shutil
import subprocess
from pathlib import Path

import pytest

from capledger_harness.sla_reference import (Contract, at_least, compute, from_statement, load_events, round_half_away,
                                             seconds, split_after, subtract, unix)

REPO = Path(__file__).resolve().parents[2]
OCT = (unix("2026-10-01T00:00:00Z"), unix("2026-11-01T00:00:00Z"))


def test_helpers():
    assert seconds("5m") == 300 and seconds("1h30m") == 5400 and seconds("1.5h") == 5400 and seconds("90s") == 90
    with pytest.raises(ValueError):
        seconds("5 minutes")
    assert subtract([(0, 10)], [(2, 3), (5, 6)]) == [(0, 2), (3, 5), (6, 10)]
    assert split_after([(0, 5), (10, 20)], 7) == ([(0, 5), (10, 12)], [(12, 20)])
    assert at_least([[(0, 10)], [(5, 15)], [(8, 9)]], 2) == [(5, 10)]
    assert round_half_away(__import__("fractions").Fraction(5, 2), 0) == "3"
    assert round_half_away(__import__("fractions").Fraction(12345, 100000), 4) == "0.1235"


def test_documented_examples():
    ex = REPO / "examples"
    hgx = compute(Contract.load(str(ex / "contracts/hgx-16-node.yaml")), load_events([str(ex / "events/hgx-16-node-2026-10.jsonl")]), OCT)
    assert hgx["downtimeNodeSeconds"] == 127680 and hgx["availabilityPercent"] == "99.7021"
    assert hgx["creditPercent"] == 5 and hgx["creditAmount"] == "15000.00"
    assert hgx["nodes"]["gpu-010"] == [0, 10800, 0]
    both = compute(Contract.load(str(ex / "contracts/hgx-16-node-reconciled.yaml")),
                   load_events([str(ex / "events/hgx-16-node-2026-10-both.jsonl")]), OCT)
    assert both["downtimeNodeSeconds"] == 130860 and both["availabilityPercent"] == "99.6946"
    assert both["nodes"]["gpu-003"] == [8940, 0, 0]


def capledger_bin():
    b = os.environ.get("CAPLEDGER_BIN") or str(REPO / "bin" / "capledger")
    return b if Path(b).is_file() and os.access(b, os.X_OK) else shutil.which("capledger")


needs_bin = pytest.mark.skipif(capledger_bin() is None, reason="set CAPLEDGER_BIN or run make build to compare with the Go engine")


def go_compute(tmp: Path, contract: dict, events: list, start: str, end: str) -> dict:
    cpath, epath = tmp / "c.json", tmp / "e.jsonl"
    cpath.write_text(json.dumps(contract))
    epath.write_text("".join(json.dumps(e) + "\n" for e in events))
    p = subprocess.run([capledger_bin(), "sla", "compute", "--contract", str(cpath), "--events", str(epath),
                        "--start", start, "--end", end], capture_output=True, text=True)
    if p.returncode != 0:
        return {"error": p.stderr.strip()}
    return from_statement(json.loads(p.stdout))


@needs_bin
@pytest.mark.parametrize("name", ["hgx-16-node.json", "nvl72-2-racks.json", "hgx-16-node-strict.yaml", "hgx-16-node-reconciled.yaml"])
def test_examples_match_the_engine(tmp_path, name):
    ex = REPO / "examples"
    events = {"nvl72-2-racks.json": ["nvl72-2026-10.jsonl"],
              "hgx-16-node-strict.yaml": ["hgx-16-node-2026-10.jsonl", "hgx-16-node-2026-10-gaps.jsonl"],
              "hgx-16-node-reconciled.yaml": ["hgx-16-node-2026-10-both.jsonl"]}.get(name, ["hgx-16-node-2026-10.jsonl"])
    paths = [str(ex / "events" / e) for e in events]
    args = [capledger_bin(), "sla", "compute", "--contract", str(ex / "contracts" / name), "--period", "2026-10"]
    for p in paths:
        args += ["--events", p]
    go = from_statement(json.loads(subprocess.run(args, capture_output=True, text=True, check=True).stdout))
    assert compute(Contract.load(str(ex / "contracts" / name)), load_events(paths), OCT) == go


TYPES = ["down", "up", "degraded_start", "degraded_end", "maintenance_start", "maintenance_end", "unmonitored_start", "unmonitored_end"]
CAUSES = ["", "xid:79", "customer:oom", "customer:reboot", "network:rail-2"]


def random_case(rng: random.Random):
    nodes = [f"n{i}" for i in range(6)]
    m = {"minIncident": rng.choice(["", "0s", "5m", "15m"]), "degradedCountsAsDown": rng.random() < 0.6}
    if rng.random() < 0.5:
        m["unmonitored"] = rng.choice(["report", "count-as-down"])
    if rng.random() < 0.4:
        m["rackRule"] = {"nodesDownThreshold": rng.choice([1, 2, 3])}
    x = {"customerCaused": rng.random() < 0.7}
    if rng.random() < 0.7:
        x["maintenanceNotice"] = rng.choice(["1h", "72h"])
    if rng.random() < 0.6:
        x["maxExcludedPerNode"] = rng.choice(["30m", "2h", "8h"])
    c = {"apiVersion": "capacity-ledger/v1alpha1", "kind": "ContractSLA", "metadata": {"id": "random", "version": 1},
         "cluster": {"gpusPerNode": 8, "nodes": nodes, "racks": [{"name": "r1", "nodes": nodes[:3]}, {"name": "r2", "nodes": nodes[3:]}]},
         "measurement": m, "exclusions": x,
         "credits": {"monthlyFee": rng.choice(["300000.00", "12345.67"]), "currency": "USD", "capPercent": rng.choice([0, 10, 30]),
                     "tiers": [{"availabilityBelow": "99.9", "creditPercent": 5}, {"availabilityBelow": "99.0", "creditPercent": 10},
                               {"availabilityBelow": "95", "creditPercent": 25}, {"availabilityBelow": "80", "creditPercent": 50}]}}
    sources = ["p"]
    if rng.random() < 0.5:
        c["sources"] = {"provider": {"sources": ["p"]}, "tenant": {"sources": ["t"]}}
        if rng.random() < 0.2:
            c["sources"]["unlisted"] = "ignore"
            sources.append("x")
        m["reconcile"] = {"count": rng.choice(["either", "both", "provider", "tenant"]),
                          "start": rng.choice(["earliest", "latest", "provider", "tenant"]),
                          "end": rng.choice(["earliest", "latest", "provider", "tenant"])}
        sources.append("t")
    start = unix("2026-10-01T00:00:00Z")
    events = []
    for i in range(rng.randint(0, 60)):
        t = start + rng.randint(-6 * 3600, 2 * 86400 + 6 * 3600)
        e = {"id": f"e{i:03d}", "ts": __import__("datetime").datetime.fromtimestamp(t, __import__("datetime").timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
             "node": rng.choice(nodes + ["stranger"]), "type": rng.choice(TYPES), "source": rng.choice(sources)}
        if rng.random() < 0.6:
            e["cause"] = rng.choice(CAUSES)
        if e["type"] == "maintenance_start" and rng.random() < 0.8:
            e["noticeTs"] = __import__("datetime").datetime.fromtimestamp(t - rng.choice([0, 1800, 3600, 7200, 300000]),
                                                                          __import__("datetime").timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        if rng.random() < 0.2:
            e["seq"] = rng.randint(1, 3)
        events.append({k: v for k, v in e.items() if v != ""})
    return c, events


@needs_bin
def test_random_months_match_the_engine(tmp_path):
    cases = int(os.environ.get("CAPLEDGER_DIFF_CASES", "300"))
    rng = random.Random(int(os.environ.get("CAPLEDGER_DIFF_SEED", "2026")))
    window = ("2026-10-01T00:00:00Z", "2026-10-03T00:00:00Z")
    period = (unix(window[0]), unix(window[1]))
    for i in range(cases):
        contract, events = random_case(rng)
        go = go_compute(tmp_path, contract, events, *window)
        try:
            ref = compute(Contract.load(str(tmp_path / "c.json")), events, period)
        except ValueError as e:
            ref = {"error": str(e)}
        if "error" in go or "error" in ref:
            assert ("error" in go) == ("error" in ref), f"case {i}: go {go} ref {ref}"
            continue
        assert ref == go, f"case {i} differs:\ncontract {json.dumps(contract)}\nevents {json.dumps(events)}\nref {ref}\ngo  {go}"

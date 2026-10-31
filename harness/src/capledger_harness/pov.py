"""Proof of value on rented clusters: plan fault-injection trials and score
the results against the pass thresholds in docs/roadmap.md.

A trial is one acceptance run (plan, run, report) with zero or more nodes
deliberately degraded by ``harness/scripts/inject_fault.sh``. The trials
file is JSON Lines, one trial per line::

    {"trial": "f1-10-r1", "fault": "F1", "magnitude": 10,
     "injected": ["gpu-007"], "report": "runs/f1-10-r1/report.json",
     "rounds": 2}

``report`` is the harness report of that run (relative to the trials file);
instead, ``flagged`` and ``nodes`` can list the flagged and tested nodes
directly. ``magnitude`` is the degradation in percent for graded faults
(F1 power cap, F2 clock lock, F5 I/O throttle) and absent for faults that
are either present or not. ``rounds`` is how many localisation rounds it
took to name the culprits, if localisation ran.
"""

from __future__ import annotations

import json
import random
from dataclasses import dataclass, field
from pathlib import Path

# Pass thresholds from docs/roadmap.md.
THRESHOLDS = {
    "recall_at_10_percent": 0.95,  # degradations of 10% or more
    "recall_at_5_percent": 0.80,  # degradations of 5% to 10%
    "false_flags_per_100_node_tests": 1.0,
    "max_localisation_rounds": 3,
}

FAULTS = {
    "F0": "none (clean run)",
    "F1": "GPU power cap",
    "F2": "GPU clocks locked below base",
    "F3": "one RDMA link down",
    "F4": "NCCL misconfiguration",
    "F5": "storage throttled with io.max",
    "F6": "synthetic XID (health-check timing drill)",
}


@dataclass
class Trial:
    trial: str
    fault: str
    injected: list[str]
    flagged: list[str]
    nodes: list[str]
    magnitude: float | None = None
    rounds: int | None = None


def load_trials(path: str, flag_levels: tuple[str, ...] = ("review", "reject")) -> list[Trial]:
    """Read a trials file. Reports are read relative to its directory."""
    base = Path(path).resolve().parent
    out: list[Trial] = []
    with open(path, encoding="utf-8") as f:
        for n, line in enumerate(f, 1):
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            d = json.loads(line)
            for key in ("trial", "fault"):
                if key not in d:
                    raise ValueError(f"line {n}: missing {key!r}")
            if d["fault"] not in FAULTS:
                raise ValueError(f"line {n}: unknown fault {d['fault']!r} (use F0 to F6)")
            flagged, nodes = d.get("flagged"), d.get("nodes")
            if "report" in d:
                with open(base / d["report"], encoding="utf-8") as rf:
                    rep = json.load(rf)
                rows = rep["nodes"]
                nodes = [r["node"] for r in rows]
                flagged = [r["node"] for r in rows if r["verdict"] in flag_levels]
            if flagged is None or nodes is None:
                raise ValueError(f"line {n}: give 'report', or both 'flagged' and 'nodes'")
            injected = list(d.get("injected", []))
            if d["fault"] == "F0" and injected:
                raise ValueError(f"line {n}: an F0 trial injects nothing")
            if d["fault"] != "F0" and not injected:
                raise ValueError(f"line {n}: name the injected node(s)")
            unknown = set(injected) - set(nodes)
            if unknown:
                raise ValueError(f"line {n}: injected node(s) {sorted(unknown)} were not tested")
            out.append(Trial(trial=d["trial"], fault=d["fault"], injected=injected, flagged=list(flagged),
                             nodes=list(nodes), magnitude=d.get("magnitude"), rounds=d.get("rounds")))
    return out


@dataclass
class Rate:
    hits: int = 0
    total: int = 0

    def add(self, hit: bool) -> None:
        self.total += 1
        self.hits += int(hit)

    def value(self) -> float | None:
        return self.hits / self.total if self.total else None


def score(trials: list[Trial]) -> dict:
    """Score trials against the thresholds. A threshold without data is
    reported as such, never as passed."""
    at10, at5, below5 = Rate(), Rate(), Rate()
    by_fault: dict[str, Rate] = {}
    false_flags, clean_tests = 0, 0
    rounds: list[int] = []
    misses, false_list = [], []
    for t in trials:
        flagged, injected = set(t.flagged), set(t.injected)
        for node in t.injected:
            hit = node in flagged
            by_fault.setdefault(t.fault, Rate()).add(hit)
            if t.magnitude is not None:
                if t.magnitude >= 10:
                    at10.add(hit)
                elif t.magnitude >= 5:
                    at5.add(hit)
                else:
                    below5.add(hit)
            else:
                at10.add(hit)  # present-or-absent faults are gross faults
            if not hit:
                misses.append({"trial": t.trial, "node": node, "fault": t.fault, "magnitude": t.magnitude})
        for node in t.nodes:
            if node in injected:
                continue
            clean_tests += 1
            if node in flagged:
                false_flags += 1
                false_list.append({"trial": t.trial, "node": node, "fault": t.fault})
        if t.rounds is not None:
            rounds.append(t.rounds)

    def check(value, threshold, higher_is_better=True):
        if value is None:
            return {"value": None, "threshold": threshold, "result": "no data"}
        ok = value >= threshold if higher_is_better else value <= threshold
        return {"value": round(value, 4), "threshold": threshold, "result": "pass" if ok else "fail"}

    ff = 100.0 * false_flags / clean_tests if clean_tests else None
    checks = {
        "recall_at_10_percent": check(at10.value(), THRESHOLDS["recall_at_10_percent"]),
        "recall_at_5_percent": check(at5.value(), THRESHOLDS["recall_at_5_percent"]),
        "false_flags_per_100_node_tests": check(ff, THRESHOLDS["false_flags_per_100_node_tests"], higher_is_better=False),
        "max_localisation_rounds": check(max(rounds) if rounds else None, THRESHOLDS["max_localisation_rounds"], higher_is_better=False),
    }
    results = [c["result"] for c in checks.values()]
    return {
        "trials": len(trials),
        "checks": checks,
        "verdict": "fail" if "fail" in results else ("incomplete" if "no data" in results else "pass"),
        "recall_by_fault": {k: {"detected": r.hits, "injected": r.total} for k, r in sorted(by_fault.items())},
        "recall_below_5_percent": {"detected": below5.hits, "injected": below5.total},
        "clean_node_tests": clean_tests,
        "false_flags": false_list,
        "misses": misses,
    }


def to_text(s: dict) -> str:
    lines = [f"{s['trials']} trial(s); verdict: {s['verdict'].upper()}", ""]
    names = {
        "recall_at_10_percent": "recall, degradation >= 10%",
        "recall_at_5_percent": "recall, degradation 5-10%",
        "false_flags_per_100_node_tests": "false flags per 100 node-tests",
        "max_localisation_rounds": "localisation rounds (worst)",
    }
    for key, c in s["checks"].items():
        v = "-" if c["value"] is None else c["value"]
        lines.append(f"  {names[key]:<34} {str(v):>8}  (threshold {c['threshold']})  {c['result']}")
    lines.append("")
    for fault, r in s["recall_by_fault"].items():
        lines.append(f"  {fault} {FAULTS[fault]:<44} {r['detected']}/{r['injected']} detected")
    for m in s["misses"]:
        lines.append(f"  missed: {m['trial']} {m['node']} ({m['fault']}, {m['magnitude']}%)")
    for f in s["false_flags"]:
        lines.append(f"  false flag: {f['trial']} {f['node']}")
    return "\n".join(lines) + "\n"


# The trial matrix from docs/roadmap.md: ten clean runs, three power caps,
# and one run of each other fault, each repeated.
MATRIX = [
    ("F0", None, 10),
    ("F1", 5, 1), ("F1", 10, 1), ("F1", 20, 1),
    ("F2", 10, 1),
    ("F3", None, 1),
    ("F4", None, 1),
    ("F5", 10, 1),
    ("F6", None, 1),
]


@dataclass
class PlannedTrial:
    trial: str
    fault: str
    magnitude: float | None
    injected: list[str]
    inject: list[str] = field(default_factory=list)
    restore: list[str] = field(default_factory=list)


def inject_commands(fault: str, magnitude: float | None, node: str) -> list[str]:
    """Shell commands (run on the node, as root) that inject one fault.
    Placeholders in capitals must be filled in for the cluster."""
    tool = "sudo ./inject_fault.sh --yes --truth injections.jsonl --node " + node
    if fault == "F1":
        return [f"{tool} power-cap {magnitude:g}"]
    if fault == "F2":
        return [f"{tool} lock-clocks BASE_CLOCK_MINUS_{magnitude:g}_PERCENT_MHZ"]
    if fault == "F3":
        return [f"{tool} link-down RDMA_NETDEV"]
    if fault == "F4":
        return [f"{tool} nccl-misconfig NCCL_IB_GID_INDEX=WRONG_INDEX"]
    if fault == "F5":
        return [f"{tool} io-throttle JOB_CGROUP STORAGE_MAJ:MIN THROTTLED_BYTES_PER_SECOND"]
    if fault == "F6":
        return [f"{tool} xid-drill 79 GPU_PCI_ADDRESS"]
    return []


def plan_trials(nodes: list[str], repeats: int, seed: int) -> list[PlannedTrial]:
    """Pick a victim node per trial, deterministically from seed."""
    if len(nodes) < 2:
        raise ValueError("the proof needs at least two nodes")
    rng = random.Random(seed)
    out: list[PlannedTrial] = []
    for fault, magnitude, runs in MATRIX:
        for r in range(1, runs * repeats + 1):
            mag = "" if magnitude is None else f"-{magnitude:g}"
            name = f"{fault.lower()}{mag}-r{r}"
            if fault == "F0":
                out.append(PlannedTrial(name, fault, None, []))
                continue
            victim = rng.choice(nodes)
            restore = [] if fault == "F6" else ["sudo ./inject_fault.sh --yes --truth injections.jsonl --node " + victim + " restore"]
            out.append(PlannedTrial(name, fault, magnitude, [victim], inject_commands(fault, magnitude, victim), restore))
    return out

"""Turn raw acceptance results into per-node verdicts.

Verdicts:

* ``reject``  the node failed a functional check (missing GPUs, DCGM failure,
  wrong collective results) and should be replaced before acceptance.
* ``review``  the node works but something needs a decision: it is slow
  against its peers, or it sits in a collective group that missed its floor
  and has not yet been localised.
* ``accept``  no problems found by the tests that ran.

"accept" means "no defect found by these tests", not "no defect exists".
Silent data corruption and slow-onset faults need longer observation.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timezone

import yaml

from .baseline import find_outliers
from .parsers.amdgpu import count_amd_gpus, rvs_failed
from .parsers.nccl import parse_nccl_output
from .parsers.nvsmi import count_gpus

ORDER = {"accept": 0, "review": 1, "reject": 2}
# Single-node diagnostics by task kind: DCGM (NVIDIA), the ROCm Validation
# Suite (AMD) or the inventory's own single_node_command.
DIAGNOSTICS = {"dcgm": "DCGM diagnostics", "rvs": "ROCm Validation Suite", "diag": "single-node diagnostic"}


@dataclass
class Criteria:
    """Pass thresholds. Calibrate them per GPU, NIC and topology."""

    nccl_min_busbw_gbps: dict[str, dict[int, float]] = field(default_factory=dict)
    # Reject nodes whose single-node diagnostic (DCGM, RVS or a custom
    # command) fails. The name predates AMD support.
    require_dcgm_pass: bool = True
    outlier_z: float = 3.5
    outlier_rel: float = 0.05

    @staticmethod
    def load(path: str) -> "Criteria":
        with open(path, encoding="utf-8") as f:
            d = yaml.safe_load(f) or {}
        floors = {op: {int(k): float(v) for k, v in sizes.items()} for op, sizes in (d.get("nccl_min_busbw_gbps") or {}).items()}
        return Criteria(
            nccl_min_busbw_gbps=floors,
            require_dcgm_pass=bool(d.get("require_dcgm_pass", True)),
            outlier_z=float(d.get("outlier_z", 3.5)),
            outlier_rel=float(d.get("outlier_rel", 0.05)),
        )


def build_report(
    results: list[dict],
    criteria: Criteria,
    *,
    cluster: str,
    expected_gpus: int,
    node_metrics: dict[str, dict[str, float]] | None = None,
) -> dict:
    nodes = sorted({n for r in results for n in r["nodes"]})
    verdict = {n: "accept" for n in nodes}
    reasons: dict[str, list[str]] = {n: [] for n in nodes}

    def flag(node: str, level: str, why: str) -> None:
        reasons.setdefault(node, []).append(why)
        if ORDER[level] > ORDER[verdict.get(node, "accept")]:
            verdict[node] = level

    groups = []
    skipped = sum(1 for r in results if r["status"] == "skipped")
    tested: set[str] = set()
    for r in results:
        if r["status"] == "skipped":
            continue
        kind, rnodes = r["kind"], r["nodes"]
        tested.update(rnodes)
        if r["status"] == "timeout":
            for n in rnodes:
                flag(n, "review", f"{r['task_id']} timed out")
            continue
        if kind == "audit":
            amd = (r.get("meta") or {}).get("vendor") == "amd"
            found = count_amd_gpus(r["stdout"]) if amd else count_gpus(r["stdout"])
            if found < expected_gpus:
                flag(rnodes[0], "reject", f"only {found} of {expected_gpus} GPUs visible")
        elif kind in DIAGNOSTICS and criteria.require_dcgm_pass:
            if r["returncode"] != 0:
                flag(rnodes[0], "reject", f"{DIAGNOSTICS[kind]} failed (exit {r['returncode']})")
            elif kind == "rvs" and rvs_failed(r["stdout"]):
                flag(rnodes[0], "reject", f"{DIAGNOSTICS[kind]} reported a failed check")
        elif kind == "nccl":
            res = parse_nccl_output(r["stdout"])
            op, size = r["meta"].get("op"), r["meta"].get("world_nodes")
            floor = criteria.nccl_min_busbw_gbps.get(op, {}).get(size)
            busbw = res.busbw_at_max_size
            ok = r["returncode"] == 0 and bool(res.rows) and res.passed_validation and (floor is None or busbw >= floor)
            groups.append({"task": r["task_id"], "op": op, "world_nodes": size, "busbw_gbps": busbw, "floor_gbps": floor, "wrong": res.wrong_total, "pass": ok})
            if not res.passed_validation:
                for n in rnodes:
                    flag(n, "reject", f"{r['task_id']}: collective returned wrong results")
            elif not ok:
                why = f"{r['task_id']}: bus bandwidth {busbw:.1f} GB/s below floor {floor}" if floor else f"{r['task_id']}: run failed"
                for n in rnodes:
                    flag(n, "review", why + "; run localisation to find the culprit")
        elif r["status"] == "failed":
            for n in rnodes:
                flag(n, "review", f"{r['task_id']} failed (exit {r['returncode']})")

    # A node nothing ran on has not earned "accept" (dry runs, filtered stages).
    for n in nodes:
        if n not in tested:
            flag(n, "review", "no tests ran for this node")

    for o in find_outliers(node_metrics or {}, z_threshold=criteria.outlier_z, rel_threshold=criteria.outlier_rel):
        verdict.setdefault(o.node, "accept")
        flag(o.node, "review", f"{o.metric} {o.value:g}: {o.reason}")

    node_rows = [{"node": n, "verdict": verdict[n], "reasons": reasons.get(n, [])} for n in sorted(verdict)]
    summary = {k: sum(1 for row in node_rows if row["verdict"] == k) for k in ("accept", "review", "reject")}
    return {
        "apiVersion": "capacity-ledger/v1alpha1",
        "kind": "AcceptanceReport",
        "cluster": cluster,
        "generated": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "summary": summary | {"skipped_tasks": skipped},
        "nodes": node_rows,
        "collective_groups": groups,
        "limits": "accept means no defect was found by the tests that ran; it does not rule out silent data corruption or slow-onset faults",
    }


def to_markdown(report: dict) -> str:
    s = report["summary"]
    lines = [
        f"# Acceptance report: {report['cluster']}",
        "",
        f"Generated {report['generated']}. {s['accept']} accept, {s['review']} review, {s['reject']} reject; {s['skipped_tasks']} task(s) skipped.",
        "",
        "| Node | Verdict | Reasons |",
        "| --- | --- | --- |",
    ]
    for row in report["nodes"]:
        lines.append(f"| {row['node']} | {row['verdict']} | {'; '.join(row['reasons']) or '-'} |")
    if report["collective_groups"]:
        lines += ["", "| Collective | World size (nodes) | Bus bandwidth (GB/s) | Floor (GB/s) | Pass |", "| --- | --- | --- | --- | --- |"]
        for g in report["collective_groups"]:
            lines.append(f"| {g['op']} | {g['world_nodes']} | {g['busbw_gbps']:.1f} | {g['floor_gbps'] if g['floor_gbps'] is not None else '-'} | {'yes' if g['pass'] else 'no'} |")
    lines += ["", f"_{report['limits']}._", ""]
    return "\n".join(lines)

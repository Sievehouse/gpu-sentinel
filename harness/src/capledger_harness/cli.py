"""capledger-harness command line."""

from __future__ import annotations

import argparse
import json
import sys
from dataclasses import asdict

from . import __version__
from .baseline import find_outliers
from .contract_convert import convert_file
from .inventory import Inventory
from .localize import simulate
from .parsers.nccl import parse_nccl_output
from .plan import STAGES, Plan, build_plan
from .pov import THRESHOLDS, load_trials, plan_trials, score, to_text
from .report import Criteria, build_report, to_markdown
from .runner import load_results, run_plan


def _load_json(path: str):
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def cmd_plan(a: argparse.Namespace) -> int:
    plan = build_plan(Inventory.load(a.inventory))
    with open(a.out, "w", encoding="utf-8") as f:
        json.dump(plan.to_dict(), f, indent=2)
        f.write("\n")
    counts = {s: len(plan.stage(s)) for s in STAGES}
    print(f"wrote {a.out}: {len(plan.tasks)} tasks " + ", ".join(f"{s}={n}" for s, n in counts.items() if n))
    return 0


def cmd_run(a: argparse.Namespace) -> int:
    plan = Plan.from_dict(_load_json(a.plan))
    results = run_plan(plan, a.out, dry_run=a.dry_run, stages=a.stage or None)
    if a.dry_run:
        for r in results:
            print(f"[{r.stage}] {r.command}")
    bad = [r for r in results if r.status in ("failed", "timeout")]
    print(f"wrote {a.out}: {len(results)} results, {len(bad)} failed or timed out", file=sys.stderr)
    return 1 if bad else 0


def cmd_report(a: argparse.Namespace) -> int:
    criteria = Criteria.load(a.criteria) if a.criteria else Criteria()
    metrics = _load_json(a.metrics) if a.metrics else None
    rep = build_report(load_results(a.results), criteria, cluster=a.cluster, expected_gpus=a.expected_gpus, node_metrics=metrics)
    with open(a.out, "w", encoding="utf-8") as f:
        json.dump(rep, f, indent=2)
        f.write("\n")
    if a.markdown:
        with open(a.markdown, "w", encoding="utf-8") as f:
            f.write(to_markdown(rep))
    s = rep["summary"]
    print(f"{s['accept']} accept, {s['review']} review, {s['reject']} reject -> {a.out}")
    return 0


def cmd_localize_sim(a: argparse.Namespace) -> int:
    print(json.dumps(simulate(a.nodes, a.bad, a.degradation, a.noise, a.seed, a.fanout), indent=2))
    return 0


def cmd_outliers(a: argparse.Namespace) -> int:
    found = find_outliers(_load_json(a.metrics), z_threshold=a.z, rel_threshold=a.rel)
    print(json.dumps([asdict(o) for o in found], indent=2))
    return 0


def cmd_parse_nccl(a: argparse.Namespace) -> int:
    with open(a.file, encoding="utf-8") as f:
        res = parse_nccl_output(f.read())
    print(json.dumps({
        "rows": len(res.rows), "ranks": res.ranks, "avg_busbw_gbps": res.avg_busbw,
        "busbw_at_max_size_gbps": res.busbw_at_max_size, "peak_busbw_gbps": res.peak_busbw,
        "wrong_total": res.wrong_total, "passed_validation": res.passed_validation,
    }, indent=2))
    return 0


def cmd_convert(a: argparse.Namespace) -> int:
    for w in convert_file(a.input, a.out):
        print(f"warning: {w}", file=sys.stderr)
    print(f"wrote {a.out}")
    return 0


def cmd_pov_plan(a: argparse.Namespace) -> int:
    inv = Inventory.load(a.inventory)
    trials = plan_trials(list(inv.nodes), a.repeats, a.seed)
    with open(a.out, "w", encoding="utf-8") as f:
        f.write("# Proof-of-value trials (docs/roadmap.md). For each trial: run the inject commands on the\n")
        f.write("# victim node, run plan/run/report into the report path, run the restore commands.\n")
        f.write("# Fill in the CAPITALISED placeholders for your cluster first.\n")
        for t in trials:
            row = {"trial": t.trial, "fault": t.fault, "injected": t.injected, "report": f"runs/{t.trial}/report.json"}
            if t.magnitude is not None:
                row["magnitude"] = t.magnitude
            if t.inject:
                row["inject"], row["restore"] = t.inject, t.restore
            f.write(json.dumps(row) + "\n")
    faulty = sum(1 for t in trials if t.injected)
    print(f"wrote {a.out}: {len(trials)} trials ({len(trials) - faulty} clean, {faulty} with a fault) on {len(inv.nodes)} nodes")
    return 0


def cmd_pov_score(a: argparse.Namespace) -> int:
    levels = ("review", "reject") if a.flag_level == "review" else ("reject",)
    s = score(load_trials(a.trials, levels), max_rounds=a.max_rounds)
    if a.out:
        with open(a.out, "w", encoding="utf-8") as f:
            json.dump(s, f, indent=2)
            f.write("\n")
    print(to_text(s), end="")
    return 1 if s["verdict"] == "fail" else 0


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="capledger-harness", description="Acceptance tests for GPU clusters (GPU Sentinel).")
    p.add_argument("--version", action="version", version=f"%(prog)s {__version__}")
    sub = p.add_subparsers(dest="cmd", required=True)

    s = sub.add_parser("plan", help="build an acceptance plan from an inventory")
    s.add_argument("--inventory", required=True)
    s.add_argument("--out", default="plan.json")
    s.set_defaults(fn=cmd_plan)

    s = sub.add_parser("run", help="execute a plan (or print it with --dry-run)")
    s.add_argument("--plan", required=True)
    s.add_argument("--out", default="results.jsonl")
    s.add_argument("--dry-run", action="store_true")
    s.add_argument("--stage", action="append", choices=STAGES, help="run only this stage; repeatable")
    s.set_defaults(fn=cmd_run)

    s = sub.add_parser("report", help="per-node verdicts from results")
    s.add_argument("--results", required=True)
    s.add_argument("--cluster", required=True)
    s.add_argument("--expected-gpus", type=int, required=True)
    s.add_argument("--criteria")
    s.add_argument("--metrics", help="JSON of node -> {metric: value} for peer comparison")
    s.add_argument("--out", default="report.json")
    s.add_argument("--markdown")
    s.set_defaults(fn=cmd_report)

    s = sub.add_parser("localize-sim", help="simulate group-testing fault localisation")
    s.add_argument("--nodes", type=int, default=64)
    s.add_argument("--bad", type=int, default=2)
    s.add_argument("--degradation", type=float, default=0.20)
    s.add_argument("--noise", type=float, default=0.015)
    s.add_argument("--seed", type=int, default=7)
    s.add_argument("--fanout", type=int, default=2, help="parts a failing group is split into per round (more tests, fewer rounds)")
    s.set_defaults(fn=cmd_localize_sim)

    s = sub.add_parser("outliers", help="flag nodes slow against their peers")
    s.add_argument("--metrics", required=True)
    s.add_argument("--z", type=float, default=3.5)
    s.add_argument("--rel", type=float, default=0.05)
    s.set_defaults(fn=cmd_outliers)

    s = sub.add_parser("parse-nccl", help="summarise nccl-tests output")
    s.add_argument("file")
    s.set_defaults(fn=cmd_parse_nccl)

    s = sub.add_parser("convert-contract", help="YAML contract spec -> JSON (capledger 0.2+ also reads YAML directly)")
    s.add_argument("--in", dest="input", required=True)
    s.add_argument("--out", required=True)
    s.set_defaults(fn=cmd_convert)

    s = sub.add_parser("pov-plan", help="plan the proof-of-value fault-injection trials (F0-F6)")
    s.add_argument("--inventory", required=True)
    s.add_argument("--repeats", type=int, default=3, help="repeat every faulty trial this many times (clean runs: 10x)")
    s.add_argument("--seed", type=int, default=1)
    s.add_argument("--out", default="trials.jsonl")
    s.set_defaults(fn=cmd_pov_plan)

    s = sub.add_parser("pov-score", help="score trials against the proof-of-value thresholds")
    s.add_argument("--trials", required=True)
    s.add_argument("--flag-level", choices=("review", "reject"), default="review",
                   help="lowest verdict that counts as flagging a node (default: review)")
    s.add_argument("--max-rounds", type=int, default=THRESHOLDS["max_localisation_rounds"],
                   help="localisation rounds allowed (default: the roadmap's 3)")
    s.add_argument("--out", help="also write the score as JSON")
    s.set_defaults(fn=cmd_pov_score)

    a = p.parse_args(argv)
    return a.fn(a)


if __name__ == "__main__":
    raise SystemExit(main())

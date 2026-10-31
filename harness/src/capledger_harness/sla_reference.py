"""An independent reference implementation of the SLA rules in
docs/sla-spec.md, written in Python from the specification rather than
translated from the Go engine.

It exists to check the engine: the proof-of-value threshold "SLA engine
100% match with independent references" is tested by computing the same
contracts and events here and with ``capledger sla compute`` and comparing
every node's numbers (tests/test_sla_reference.py, thousands of random
months). It does not seal statements and is not meant for contract use.
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass, field
from datetime import datetime, timezone
from fractions import Fraction
from pathlib import Path

import yaml

Interval = tuple[int, int]

_DUR = re.compile(r"(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|ms|s|m|h)")
_UNIT = {"ns": Fraction(1, 10**9), "us": Fraction(1, 10**6), "µs": Fraction(1, 10**6), "ms": Fraction(1, 1000),
         "s": Fraction(1), "m": Fraction(60), "h": Fraction(3600)}


def seconds(d: str | None) -> int:
    """Whole seconds in a Go duration such as "5m" or "1h30m"."""
    if not d:
        return 0
    if d == "0":
        return 0
    pos, total = 0, Fraction(0)
    for m in _DUR.finditer(d):
        if m.start() != pos:
            raise ValueError(f"bad duration {d!r}")
        total += Fraction(m.group(1)) * _UNIT[m.group(2)]
        pos = m.end()
    if pos != len(d):
        raise ValueError(f"bad duration {d!r}")
    return int(total)  # truncate, as time.Duration / time.Second does


def unix(ts: str) -> int:
    """Unix seconds of an RFC 3339 timestamp, sub-seconds truncated."""
    s = re.sub(r"\.\d+", "", ts.strip()).replace("Z", "+00:00").replace("z", "+00:00")
    return int(datetime.fromisoformat(s).timestamp())


def normalize(ivs) -> list[Interval]:
    """Merge overlapping and touching intervals; drop empty ones."""
    out: list[list[int]] = []
    for a, b in sorted(iv for iv in ivs if iv[1] > iv[0]):
        if out and a <= out[-1][1]:
            out[-1][1] = max(out[-1][1], b)
        else:
            out.append([a, b])
    return [(a, b) for a, b in out]


def total(s) -> int:
    return sum(b - a for a, b in s)


def union(*sets) -> list[Interval]:
    return normalize([iv for s in sets for iv in s])


def intersect(x, y) -> list[Interval]:
    return normalize([(max(a, c), min(b, d)) for a, b in x for c, d in y])


def subtract(x, y) -> list[Interval]:
    out = []
    for a, b in x:
        pieces = [(a, b)]
        for c, d in y:
            nxt = []
            for p, q in pieces:
                if d <= p or c >= q:
                    nxt.append((p, q))
                    continue
                if p < c:
                    nxt.append((p, c))
                if d < q:
                    nxt.append((d, q))
            pieces = nxt
        out += pieces
    return normalize(out)


def at_least(sets, k: int) -> list[Interval]:
    """Instants covered by at least k of the sets."""
    edges = sorted({t for s in sets for iv in s for t in iv})
    out = []
    for a, b in zip(edges, edges[1:]):
        if sum(1 for s in sets if any(p <= a and b <= q for p, q in s)) >= k:
            out.append((a, b))
    return normalize(out)


def split_after(s, budget: int):
    """The first `budget` seconds of s (in time order), and the rest."""
    if budget < 0:
        return list(s), []
    head, tail, left = [], [], budget
    for a, b in s:
        take = min(left, b - a)
        if take > 0:
            head.append((a, a + take))
        if a + take < b:
            tail.append((a + take, b))
        left -= take
    return normalize(head), normalize(tail)


@dataclass
class Contract:
    nodes: list[str]
    racks: dict[str, list[str]]
    gpus: int
    min_incident: int
    degraded_counts: bool
    rack_threshold: int | None
    unmonitored_counts: bool
    notice: int | None  # None: maintenance is never excluded
    cap: int  # -1: no cap
    customer_caused: bool
    fee: Fraction | None
    cap_percent: int
    tiers: list[tuple[Fraction, int, str]]
    reconcile: dict | None
    sources: dict | None

    @staticmethod
    def load(path: str) -> "Contract":
        text = Path(path).read_text(encoding="utf-8")
        c = json.loads(text) if text.lstrip().startswith("{") else yaml.safe_load(text)
        m, x, cr = c.get("measurement", {}), c.get("exclusions", {}), c.get("credits", {})
        return Contract(
            nodes=sorted(c["cluster"]["nodes"]),
            racks={r["name"]: r["nodes"] for r in c["cluster"].get("racks", [])},
            gpus=c["cluster"]["gpusPerNode"],
            min_incident=seconds(m.get("minIncident")),
            degraded_counts=bool(m.get("degradedCountsAsDown")),
            rack_threshold=(m.get("rackRule") or {}).get("nodesDownThreshold"),
            unmonitored_counts=m.get("unmonitored") == "count-as-down",
            notice=seconds(x["maintenanceNotice"]) if x.get("maintenanceNotice") else None,
            cap=seconds(x["maxExcludedPerNode"]) if x.get("maxExcludedPerNode") else -1,
            customer_caused=bool(x.get("customerCaused")),
            fee=Fraction(cr["monthlyFee"]) if cr.get("monthlyFee") else None,
            cap_percent=cr.get("capPercent", 0),
            tiers=[(Fraction(t["availabilityBelow"]), t["creditPercent"], t["availabilityBelow"]) for t in cr.get("tiers", [])],
            reconcile=m.get("reconcile"),
            sources=c.get("sources"),
        )

    def side_of(self, e: dict) -> str | None:
        if not self.sources:
            return "provider"
        for side in ("provider", "tenant"):
            spec = self.sources.get(side) or {}
            if e.get("signerRole"):
                if e["signerRole"] in spec.get("roles", []):
                    return side
            elif e.get("source", "") in spec.get("sources", []):
                return side
        return None


@dataclass
class Segment:
    start: int
    end: int
    side: str
    kind: str
    excluded: bool = False
    report_only: bool = False


def replay(c: Contract, evs: list[dict], period: Interval, side: str) -> list[Segment]:
    """One party's events for one node, as classified segments."""
    p0, p1 = period
    open_: dict[str, dict] = {}
    out: list[Segment] = []
    starts = {"down": "down", "degraded_start": "degraded", "maintenance_start": "maintenance", "unmonitored_start": "unmonitored"}
    ends = {"up": "down", "degraded_end": "degraded", "maintenance_end": "maintenance", "unmonitored_end": "unmonitored"}

    def close(kind: str, end: int) -> None:
        o = open_.pop(kind)
        a, b = max(o["start"], p0), min(end, p1)
        if b <= a:
            return
        seg = Segment(a, b, side, kind)
        if kind == "down":
            causes = [x for x in o["causes"] if x]
            seg.excluded = c.customer_caused and bool(causes) and all(x.startswith("customer") for x in causes)
        elif kind == "degraded":
            if not c.degraded_counts:
                return
        elif kind == "maintenance":
            n = o["notice"]
            seg.excluded = c.notice is not None and n is not None and o["start"] - n >= c.notice
        elif kind == "unmonitored":
            seg.report_only = not c.unmonitored_counts
        out.append(seg)

    for e in evs:
        t, typ = unix(e["ts"]), e["type"]
        if typ in starts:
            kind = starts[typ]
            if kind not in open_:
                notice = unix(e["noticeTs"]) if e.get("noticeTs") else None
                open_[kind] = {"start": t, "causes": [], "notice": notice}
            open_[kind]["causes"].append(e.get("cause", ""))
        elif typ in ends and ends[typ] in open_:
            close(ends[typ], t)
    for kind in list(open_):
        close(kind, p1)
    return out


def reconcile(rule: dict, prov, ten) -> list[Interval]:
    out = []
    for a, b in union(prov, ten):
        pc, tc = intersect(prov, [(a, b)]), intersect(ten, [(a, b)])
        count = rule["count"]
        if (count == "both" and not (pc and tc)) or (count == "provider" and not pc) or (count == "tenant" and not tc):
            continue

        def pick(which: str, first: bool) -> int:
            edge = (lambda s: s[0][0]) if first else (lambda s: s[-1][1])
            if which == "provider":
                return edge(pc if pc else tc)
            if which == "tenant":
                return edge(tc if tc else pc)
            if not (pc and tc):
                return a if first else b
            return max(edge(pc), edge(tc)) if which == "latest" else min(edge(pc), edge(tc))

        s, e = pick(rule["start"], True), pick(rule["end"], False)
        if e > s:
            out.append((s, e))
    return normalize(out)


def round_half_away(x: Fraction, places: int) -> str:
    scaled = x * 10**places
    n = int(abs(scaled) + Fraction(1, 2))  # half away from zero
    n = -n if scaled < 0 else n
    sign = "-" if n < 0 else ""
    n = abs(n)
    whole, frac = divmod(n, 10**places)
    return f"{sign}{whole}.{frac:0{places}d}" if places else f"{sign}{whole}"


def compute(c: Contract, events: list[dict], period: Interval) -> dict:
    """Per-node downtime, exclusions and unmonitored time, and the summary."""
    evs = sorted(events, key=lambda e: (unix(e["ts"]), e.get("seq", 0), e["id"]))
    by_node: dict[str, list[tuple[str, dict]]] = {}
    for e in evs:
        side = c.side_of(e)
        if side is None:
            if (c.sources or {}).get("unlisted") == "ignore":
                continue
            raise ValueError(f"event {e['id']}: source not listed")
        if e["node"] in c.nodes:
            by_node.setdefault(e["node"], []).append((side, e))

    node_down, allowed_excl, unmonitored = {}, {}, {}
    for n in c.nodes:
        mine = by_node.get(n, [])
        if c.reconcile:
            segs = replay(c, [e for s, e in mine if s == "provider"], period, "provider") + \
                replay(c, [e for s, e in mine if s == "tenant"], period, "tenant")
        else:
            segs = replay(c, [e for _, e in mine], period, "provider")
        countable = {"provider": [], "tenant": []}
        excluded, unmon = [], []
        for sg in segs:
            if sg.kind == "unmonitored":
                unmon.append((sg.start, sg.end))
            if sg.report_only:
                continue
            (excluded if sg.excluded else countable[sg.side]).append((sg.start, sg.end))
        if c.reconcile:
            raw = reconcile(c.reconcile, normalize(countable["provider"]), normalize(countable["tenant"]))
        else:
            raw = normalize(countable["provider"])
        allowed, overflow = split_after(normalize(excluded), c.cap)
        down = union(subtract(raw, allowed), overflow)
        node_down[n] = [iv for iv in down if iv[1] - iv[0] >= c.min_incident]
        allowed_excl[n], unmonitored[n] = allowed, normalize(unmon)

    final = dict(node_down)
    if c.rack_threshold:
        for rack, members in sorted(c.racks.items()):
            rack_down = at_least([node_down[m] for m in members], c.rack_threshold)
            rack_down = [iv for iv in rack_down if iv[1] - iv[0] >= c.min_incident]
            for m in members:
                final[m] = union(final[m], rack_down)

    nodes = {}
    down_total = 0
    for n in c.nodes:
        row = (total(final[n]), total(allowed_excl[n]), total(unmonitored[n]))
        down_total += row[0]
        if any(row):
            nodes[n] = list(row)
    span = len(c.nodes) * (period[1] - period[0])
    avail = Fraction(span - down_total, span) * 100
    best = None
    for below, pct, text in c.tiers:
        if avail >= below:
            continue
        if best is None or pct > best[1] or (pct == best[1] and below < best[0]):
            best = (below, pct, text)
    pct = best[1] if best else 0
    if c.cap_percent and pct > c.cap_percent:
        pct = c.cap_percent
    amount = round_half_away(c.fee * pct / 100, 2) if c.fee is not None else None
    return {
        "nodes": nodes,
        "downtimeNodeSeconds": down_total,
        "excludedNodeSeconds": sum(total(v) for v in allowed_excl.values()),
        "unmonitoredNodeSeconds": sum(total(v) for v in unmonitored.values()),
        "availabilityPercent": round_half_away(avail, 4),
        "creditPercent": pct,
        "creditAmount": amount,
    }


def load_events(paths: list[str]) -> list[dict]:
    evs = []
    for p in paths:
        with open(p, encoding="utf-8") as f:
            for line in f:
                line = line.strip()
                if line and not line.startswith("#"):
                    evs.append(json.loads(line))
    return evs


def from_statement(st: dict) -> dict:
    """The same summary, read from a capledger statement, for comparison."""
    return {
        "nodes": {n["node"]: [n["downtimeSeconds"], n["excludedSeconds"], n.get("unmonitoredSeconds", 0)] for n in st["nodes"]},
        "downtimeNodeSeconds": st["summary"]["downtimeNodeSeconds"],
        "excludedNodeSeconds": st["summary"]["excludedNodeSeconds"],
        "unmonitoredNodeSeconds": st["summary"].get("unmonitoredNodeSeconds", 0),
        "availabilityPercent": st["summary"]["availabilityPercent"],
        "creditPercent": st["credit"]["percent"],
        "creditAmount": st["credit"].get("amount"),
    }

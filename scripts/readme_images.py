#!/usr/bin/env python3
"""Generate the README images for GPU Sentinel in light and dark variants.

Every number comes from the repository's example data (the outputs of
`capledger sla compute`, `capledger sla corroborate` and
`capledger-harness localize-sim`), so the pictures match the README text.

Usage: python3 scripts/readme_images.py docs/images
"""
import math
import os
import sys
from xml.sax.saxutils import escape

SANS = "system-ui, -apple-system, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif"
MONO = "ui-monospace, SFMono-Regular, 'SF Mono', Menlo, Consolas, 'Liberation Mono', monospace"

THEMES = {
    "light": dict(
        surface="#fcfcfb", plane="#f3f2ee", ink="#0b0b0b", ink2="#52514e", muted="#898781",
        grid="#e1e0d9", axis="#c3c2b7", border="#e1e0d9",
        s1="#2a78d6", s2="#eb6834", s3="#1baf7a",
        crit="#d03b3b", good="#0ca30c", neutral="#c3c2b7", neutral_ink="#52514e",
        healthy="#cde2fb", healthy_ink="#184f95",
        tiers=["#86b6ef", "#5598e7", "#256abf", "#104281"],
        tier_ink=["#0b0b0b", "#ffffff", "#ffffff", "#ffffff"],
        on_fill="#ffffff",
    ),
    "dark": dict(
        surface="#1a1a19", plane="#232321", ink="#f0efec", ink2="#c3c2b7", muted="#898781",
        grid="#2c2c2a", axis="#383835", border="#383835",
        s1="#3987e5", s2="#d95926", s3="#199e70",
        crit="#d03b3b", good="#0ca30c", neutral="#52514e", neutral_ink="#c3c2b7",
        healthy="#184f95", healthy_ink="#cde2fb",
        tiers=["#184f95", "#256abf", "#3987e5", "#86b6ef"],
        tier_ink=["#ffffff", "#ffffff", "#ffffff", "#0b0b0b"],
        on_fill="#ffffff",
    ),
}


def text(x, y, s, size=14, fill="#000", weight=400, anchor="start", family=SANS, extra=""):
    return (f'<text x="{x:.1f}" y="{y:.1f}" font-family="{family}" font-size="{size}" '
            f'font-weight="{weight}" fill="{fill}" text-anchor="{anchor}"{extra}>{escape(s)}</text>')


def rect(x, y, w, h, fill, rx=0, extra=""):
    return f'<rect x="{x:.1f}" y="{y:.1f}" width="{max(w, 0):.1f}" height="{h:.1f}" rx="{rx}" fill="{fill}"{extra}/>'


def line(x1, y1, x2, y2, stroke, width=1, extra=""):
    return f'<line x1="{x1:.1f}" y1="{y1:.1f}" x2="{x2:.1f}" y2="{y2:.1f}" stroke="{stroke}" stroke-width="{width}"{extra}/>'


def svg(w, h, t, body, title, desc):
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{w}" height="{h}" viewBox="0 0 {w} {h}" '
            f'role="img" aria-labelledby="t d">\n<title id="t">{escape(title)}</title>\n'
            f'<desc id="d">{escape(desc)}</desc>\n'
            f'{rect(0.5, 0.5, w - 1, h - 1, t["surface"], rx=12, extra=f" stroke=\"{t["border"]}\"")}\n'
            + "\n".join(body) + "\n</svg>\n")


def hbar(x, y, w, h, fill):
    """Horizontal bar anchored at x with a 4px rounded data end."""
    if w <= 0:
        return ""
    r = min(4, w / 2, h / 2)
    return (f'<path d="M{x:.1f},{y:.1f} H{x + w - r:.1f} Q{x + w:.1f},{y:.1f} {x + w:.1f},{y + r:.1f} '
            f'V{y + h - r:.1f} Q{x + w:.1f},{y + h:.1f} {x + w - r:.1f},{y + h:.1f} H{x:.1f} Z" fill="{fill}"/>')


def hbar_left(x, y, w, h, fill):
    """Bar anchored at x growing left, rounded on its left (data) end."""
    if w <= 0:
        return ""
    r = min(4, w / 2, h / 2)
    return (f'<path d="M{x:.1f},{y:.1f} H{x - w + r:.1f} Q{x - w:.1f},{y:.1f} {x - w:.1f},{y + r:.1f} '
            f'V{y + h - r:.1f} Q{x - w:.1f},{y + h:.1f} {x - w + r:.1f},{y + h:.1f} H{x:.1f} Z" fill="{fill}"/>')


# ---------------------------------------------------------------- banner
NODES = [f"gpu-{i:03d}" for i in range(1, 17)]
DOWN = {"gpu-003", "gpu-005", "gpu-011", "gpu-014", "gpu-016"}
EXCLUDED = {"gpu-010", "gpu-012"}


def banner(t):
    W, H = 1280, 360
    b = []
    b.append(text(64, 92, "OPEN SOURCE  ·  APACHE-2.0  ·  PRE-ALPHA v0.2", 14, t["muted"], 600,
                  extra=' letter-spacing="1.5"'))
    b.append(text(60, 168, "GPU Sentinel", 76, t["ink"], 700, extra=' letter-spacing="-1.5"'))
    b.append(text(64, 214, "Signed, replayable evidence of the GPU capacity", 24, t["ink2"]))
    b.append(text(64, 246, "a cluster actually delivered.", 24, t["ink2"]))
    chips = ["Acceptance bundles", "Sealed SLA statements", "Provider ↔ tenant corroboration"]
    x = 64
    for c in chips:
        w = 18 + len(c) * 8.1 + 18
        b.append(rect(x, 280, w, 34, t["plane"], rx=17, extra=f' stroke="{t["border"]}"'))
        b.append(text(x + w / 2, 302, c, 15, t["ink"], 500, "middle"))
        x += w + 10

    # right-hand card: the example month as a node grid
    cx, cy, cw, ch = 806, 40, 434, 280
    b.append(rect(cx, cy, cw, ch, t["plane"], rx=10, extra=f' stroke="{t["border"]}"'))
    b.append(text(cx + 20, cy + 32, "October 2026  ·  16 nodes × 8 GPUs", 15, t["ink"], 600))
    b.append(text(cx + cw - 20, cy + 32, "example data", 13, t["muted"], 400, "end"))
    tw, th, gap = 92, 34, 8
    gx, gy = cx + 20, cy + 50
    for i, n in enumerate(NODES):
        r, c = divmod(i, 4)
        x0, y0 = gx + c * (tw + gap), gy + r * (th + gap)
        if n in DOWN:
            fill, ink = t["crit"], "#ffffff"
        elif n in EXCLUDED:
            fill, ink = t["neutral"], t["neutral_ink"]
        else:
            fill, ink = t["healthy"], t["healthy_ink"]
        b.append(rect(x0, y0, tw, th, fill, rx=6))
        mark = "×" if n in DOWN else ("–" if n in EXCLUDED else "")
        label = f"{n} {mark}".strip()
        b.append(text(x0 + tw / 2, y0 + 22, label, 13, ink, 600, "middle", MONO))
    ly = gy + 4 * (th + gap) + 14
    legend = [(t["healthy"], "up"), (t["crit"], "× counted downtime"), (t["neutral"], "– excluded")]
    lx = gx
    for col, lab in legend:
        b.append(rect(lx, ly - 10, 12, 12, col, rx=3))
        b.append(text(lx + 18, ly, lab, 13, t["ink2"]))
        lx += 26 + len(lab) * 7.2 + 14
    b.append(text(gx, ly + 34, "99.7021%", 26, t["ink"], 700))
    b.append(text(gx + 130, ly + 34, "available  →  5% credit = $15,000", 15, t["ink2"]))
    b.append(text(gx, ly + 34 + 0, "", 1, t["ink"]))
    return svg(W, H, t, b, "GPU Sentinel",
               "GPU Sentinel: signed, replayable evidence of delivered GPU capacity. The example month shows 16 nodes: "
               "11 up, 5 with counted downtime (gpu-003, 005, 011, 014, 016) and 2 with excluded time (gpu-010, 012); "
               "availability 99.7021%, 5% credit of $15,000.")


# ---------------------------------------------------------------- terminal
TERM = [
    ("cmd", "$ capledger sla compute --contract hgx-16-node.yaml --period 2026-10 \\"),
    ("cmd2", "    --events hgx-16-node-2026-10.jsonl --format text"),
    ("", ""),
    ("kv", "Contract      example-neocloud--example-lab--2026 (version 1)"),
    ("kv", "Period        2026-10-01T00:00:00Z to 2026-11-01T00:00:00Z"),
    ("kv", "Nodes         16 x 8 GPUs"),
    ("hi", "Availability  99.7021%"),
    ("kv", "Downtime      283.73 GPU-hours across 5 node(s); 16200 node-seconds excluded"),
    ("hi", "Credit        5% (availability below 99.9%) = 15000.00 USD"),
    ("", ""),
    ("head", "Node            Downtime(s)  Excluded(s)  Incidents"),
    ("row", "gpu-003                8880            0  2026-10-03T04:12:00Z 8880s [node xid:79]"),
    ("row", "gpu-005                3600            0  2026-10-01T00:00:00Z 3600s [node xid:48]"),
    ("row", "gpu-010                   0        10800"),
    ("row", "gpu-011                7200            0  2026-10-20T01:00:00Z 7200s [node emergency-patch unannounced-maintenance]"),
    ("row", "gpu-012                   0         5400"),
    ("row", "gpu-014               14400            0  2026-10-25T12:00:00Z 14400s [node nccl-busbw-below-floor]"),
    ("row", "gpu-016               93600            0  2026-10-30T22:00:00Z 93600s [node network:rail-3-link-down]"),
    ("", ""),
    ("hash", "statementSha256 d6280aea2086f9bf153922421dffbc4d44fbe17cc1106ba5184cad29f99323fc"),
]


def terminal(t):
    W = 1000
    lh = 22
    top = 56
    H = top + len(TERM) * lh + 26
    b = [rect(0.5, 0.5, W - 1, 40, t["plane"], rx=12), rect(0.5, 28, W - 1, 13, t["plane"]),
         line(0, 40.5, W, 40.5, t["border"])]
    for i in range(3):
        b.append(f'<circle cx="{24 + i * 20}" cy="20.5" r="6" fill="{t["axis"]}"/>')
    b.append(text(W / 2, 26, "capledger sla compute  ·  October 2026", 13, t["muted"], 500, "middle"))
    y = top + 14
    for kind, s in TERM:
        if kind == "cmd":
            b.append(text(24, y, "$", 13, t["s1"], 700, family=MONO, extra=' xml:space="preserve"'))
            b.append(text(38, y, s[2:], 13, t["ink"], 600, family=MONO, extra=' xml:space="preserve"'))
        elif kind == "cmd2":
            b.append(text(38, y, s, 13, t["ink"], 600, family=MONO, extra=' xml:space="preserve"'))
        elif kind == "kv" or kind == "hi":
            k, v = s[:14], s[14:]
            b.append(text(24, y, k, 13, t["muted"], 400, family=MONO, extra=' xml:space="preserve"'))
            b.append(text(24 + 14 * 7.83, y, v, 13, t["ink"], 700 if kind == "hi" else 400, family=MONO,
                          extra=' xml:space="preserve"'))
        elif kind == "head":
            b.append(text(24, y, s, 13, t["muted"], 600, family=MONO, extra=' xml:space="preserve"'))
        elif kind == "row":
            b.append(text(24, y, s, 13, t["ink2"], 400, family=MONO, extra=' xml:space="preserve"'))
        elif kind == "hash":
            b.append(text(24, y, "statementSha256", 13, t["muted"], 400, family=MONO))
            b.append(text(24 + 16 * 7.83, y, s.split()[1], 13, t["good"] if t is THEMES["dark"] else "#006300",
                          600, family=MONO))
        y += lh
    return svg(W, H, t, b, "capledger sla compute output",
               "Terminal output of capledger sla compute for the example October 2026 month: availability 99.7021%, "
               "credit 5% = 15000.00 USD, per-node downtime table and the statement hash d6280aea….")


# ---------------------------------------------------------------- SLA month
TIERS = [  # (low, high, credit label)
    (98.8, 99.0, "25%"), (99.0, 99.5, "10%"), (99.5, 99.9, "5%"), (99.9, 100.0, "0%"),
]
NODE_HOURS = [  # node, seconds, excluded?, cause
    ("gpu-016", 93600, False, "rail-3 link down"),
    ("gpu-014", 14400, False, "NCCL bus bandwidth below floor"),
    ("gpu-003", 8880, False, "XID 79, GPU fell off the bus"),
    ("gpu-011", 7200, False, "unannounced emergency patch"),
    ("gpu-005", 3600, False, "XID 48, carried over from September"),
    ("gpu-010", 10800, True, "firmware update, 4 days' notice"),
    ("gpu-012", 5400, True, "customer-caused (OOM reboot)"),
]


def sla_month(t):
    W, H = 960, 590
    b = []
    b.append(text(32, 44, "One month, one sealed statement", 20, t["ink"], 650))
    b.append(text(32, 70, "Example contract: 16 nodes, $300,000 a month, credit tiers below 99.9%, 99.5% and 99.0%",
                  14, t["ink2"]))
    # tier strip
    x0, x1, y, h = 32, W - 32, 128, 40
    lo, hi = 98.8, 100.0
    X = lambda v: x0 + (v - lo) / (hi - lo) * (x1 - x0)
    for i, (a, c, lab) in enumerate(TIERS):
        xa, xc = X(a) + (1 if i else 0), X(c) - (1 if i < 3 else 0)
        rx = 6 if i in (0, 3) else 0
        b.append(rect(xa, y, xc - xa, h, t["tiers"][3 - i], rx=rx))
        mid = (xa + xc) / 2
        mk = X(99.7021)
        if xa < mk < xc:
            mid = (xa + mk) / 2 if mk - xa > xc - mk else (mk + xc) / 2
        b.append(text(mid, y + 26, f"{lab} credit" if lab != "0%" else "no credit", 14,
                      t["tier_ink"][3 - i], 600, "middle"))
    for v in (99.0, 99.5, 99.9, 100.0):
        b.append(line(X(v), y + h, X(v), y + h + 6, t["axis"]))
        b.append(text(X(v), y + h + 22, f"{v:.1f}%", 13, t["muted"], 400, "middle"))
    b.append(text(x0, y + h + 22, "availability →", 13, t["muted"]))
    m = X(99.7021)
    b.append(line(m, y - 14, m, y + h + 4, t["ink"], 2))
    b.append(f'<circle cx="{m:.1f}" cy="{y - 14}" r="5" fill="{t["ink"]}"/>')
    b.append(text(m - 10, y - 24, "this month: 99.7021%  →  5% of $300,000 = $15,000", 14, t["ink"], 650, "end"))

    # node bars
    top = 236
    b.append(text(32, top, "Where the downtime came from (hours)", 16, t["ink"], 650))
    b.append(text(W - 32, top, "9 other nodes: none", 13, t["muted"], 400, "end"))
    lx, bx, bw = 32, 120, 380
    maxh = 26.0
    row = 38
    for i, (n, s, exc, cause) in enumerate(NODE_HOURS):
        yy = top + 24 + i * row + (14 if exc else 0)
        if i == 5:
            b.append(line(lx, yy - 12, W - 32, yy - 12, t["grid"]))
            b.append(text(lx, yy - 20 + 0, "", 1, t["ink"]))
        hrs = s / 3600
        w = hrs / maxh * bw
        b.append(text(lx, yy + 16, n, 13, t["ink2"], 500, family=MONO))
        b.append(hbar(bx, yy + 2, w, 20, t["neutral"] if exc else t["s1"]))
        lab = f"{hrs:.2f}".rstrip("0").rstrip(".") + " h"
        b.append(text(bx + w + 8, yy + 17, lab, 13, t["ink"], 650))
        b.append(text(bx + w + 8 + len(lab) * 7.6 + 8, yy + 17,
                      ("excluded · " if exc else "") + cause, 13, t["ink2"]))
    ly = top + 24 + 7 * row + 30
    b.append(rect(lx, ly - 10, 12, 12, t["s1"], rx=3))
    b.append(text(lx + 18, ly, "counts against the SLA", 13, t["ink2"]))
    b.append(rect(lx + 200, ly - 10, 12, 12, t["neutral"], rx=3))
    b.append(text(lx + 218, ly, "excluded by the contract (notice ≥ 72h, customer-caused)", 13, t["ink2"]))
    return svg(W, H, t, b, "One month, one sealed statement",
               "Availability 99.7021% falls in the 99.5–99.9% tier, so the credit is 5% of $300,000 = $15,000. "
               "Counted downtime: gpu-016 26 h, gpu-014 4 h, gpu-003 2.47 h, gpu-011 2 h, gpu-005 1 h. "
               "Excluded: gpu-010 3 h (announced firmware update), gpu-012 1.5 h (customer-caused).")


# ---------------------------------------------------------------- corroboration
DISPUTES = [  # node, provider-only s, tenant-only s, note
    ("gpu-012", 0, 5400, "cause disputed: customer OOM vs 0/8 GPUs visible"),
    ("gpu-009", 0, 3000, "probe saw 6/8 GPUs; provider recorded nothing"),
    ("gpu-005", 0, 120, "GPUs back 2 min after the provider closed the incident"),
    ("gpu-003", 60, 60, "one-minute sampling at both ends"),
    ("gpu-016", 40, 0, "sampling edge"),
]


def corroboration(t):
    W, H = 960, 480
    b = []
    b.append(text(32, 44, "Two vantage points, one comparison", 20, t["ink"], 650))
    b.append(text(32, 70, "capledger sla corroborate: provider agent vs the customer's own probe, same contract and month",
                  14, t["ink2"]))
    total = 127580 + 100 + 8580
    x0, x1, y, h = 32, W - 32, 100, 36
    span = x1 - x0
    wb = 127580 / total * span
    wp = max(4, 100 / total * span)
    wt = span - wb - wp
    b.append(rect(x0, y, wb - 1, h, t["s1"], rx=6))
    b.append(rect(x0 + wb - 6, y, 6, h, t["s1"]))
    b.append(rect(x0 + wb + 1, y, wp - 2, h, t["s2"]))
    b.append(rect(x0 + wb + wp + 1, y, wt - 1, h, t["s3"], rx=6))
    b.append(rect(x0 + wb + wp + 1, y, 6, h, t["s3"]))
    b.append(text(x0 + 16, y + 24, "93.63% seen by both", 15, t["on_fill"], 650))
    lx = x0
    for col, lab in ((t["s1"], "both: 127,580 s (93.63%)"), (t["s2"], "only provider: 100 s (0.07%)"),
                     (t["s3"], "only tenant: 8,580 s (6.30%)")):
        b.append(rect(lx, y + h + 14, 12, 12, col, rx=3))
        b.append(text(lx + 18, y + h + 25, lab, 13, t["ink2"], 500))
        lx += 18 + len(lab) * 7.3 + 28

    top = 210
    b.append(text(32, top, "The 5 nodes in dispute (seconds)", 16, t["ink"], 650))
    cx = 220  # centre axis: provider-only grows left, tenant-only right
    scale = 260 / 5400
    b.append(text(cx - 8, top + 26, "← only provider", 12, t["muted"], 500, "end"))
    b.append(text(cx + 8, top + 26, "only tenant →", 12, t["muted"], 500))
    rowh = 40
    for i, (n, p, ten, note) in enumerate(DISPUTES):
        yy = top + 40 + i * rowh
        b.append(text(32, yy + 16, n, 13, t["ink2"], 500, family=MONO))
        wp_ = p * scale
        wt_ = ten * scale
        if p:
            b.append(hbar_left(cx - 1, yy + 2, max(wp_, 3), 20, t["s2"]))
            b.append(text(cx - max(wp_, 3) - 8, yy + 17, f"{p:,}", 13, t["ink"], 650, "end"))
        if ten:
            b.append(hbar(cx + 1, yy + 2, max(wt_, 3), 20, t["s3"]))
        lab = f"{ten:,}" if ten else "0"
        lx = cx + 1 + max(wt_, 3) + 8 if ten else cx + 10
        b.append(text(lx, yy + 17, lab, 13, t["ink"], 650))
        b.append(text(lx + len(lab) * 7.6 + 10, yy + 17, note, 13, t["ink2"]))
    b.append(line(cx, top + 34, cx, top + 40 + len(DISPUTES) * rowh - 10, t["axis"]))
    return svg(W, H, t, b, "Two vantage points, one comparison",
               "Of 136,260 node-seconds of recorded downtime, 127,580 (93.63%) were seen by both the provider and the "
               "tenant probe, 100 only by the provider and 8,580 only by the tenant. Disputed nodes: gpu-012 5,400 s "
               "tenant-only, gpu-009 3,000 s tenant-only, gpu-005 120 s tenant-only, gpu-003 60 s each side, "
               "gpu-016 40 s provider-only.")


# ---------------------------------------------------------------- localisation
LOC = [(16, 15, 120), (32, 19, 496), (64, 23, 2016), (128, 27, 8128), (256, 31, 32640), (512, 35, 130816)]


def localisation(t):
    W, H = 960, 440
    b = []
    b.append(text(32, 44, "Finding 2 bad nodes without testing every pair", 20, t["ink"], 650))
    b.append(text(32, 70, "capledger-harness localize-sim --bad 2: group tests needed vs testing all node pairs",
                  14, t["ink2"]))
    px0, px1, py0, py1 = 96, W - 210, 120, H - 64
    xs = [n for n, _, _ in LOC]
    X = lambda n: px0 + (math.log2(n) - 4) / (math.log2(512) - 4) * (px1 - px0)
    Y = lambda v: py1 - (math.log10(v) - 1) / (6 - 1) * (py1 - py0)
    for e in range(1, 7):
        v = 10 ** e
        b.append(line(px0, Y(v), px1, Y(v), t["grid"]))
        lab = {10: "10", 100: "100", 1000: "1k", 10000: "10k", 100000: "100k", 1000000: "1M"}[v]
        b.append(text(px0 - 10, Y(v) + 4, lab, 12, t["muted"], 400, "end"))
    b.append(line(px0, py1, px1, py1, t["axis"]))
    for n in xs:
        b.append(text(X(n), py1 + 22, str(n), 12, t["muted"], 400, "middle"))
    b.append(text((px0 + px1) / 2, py1 + 44, "nodes in the cluster", 13, t["muted"], 400, "middle"))
    b.append(text(32, py0 - 16, "tests (log scale)", 12, t["muted"], 400))
    for key, col, idx, name in (("pairs", t["s2"], 2, "all pairs"), ("group", t["s1"], 1, "group testing")):
        pts = " ".join(f"{X(r[0]):.1f},{Y(r[idx]):.1f}" for r in LOC)
        b.append(f'<polyline points="{pts}" fill="none" stroke="{col}" stroke-width="2" '
                 f'stroke-linejoin="round" stroke-linecap="round"/>')
        for r in LOC:
            b.append(f'<circle cx="{X(r[0]):.1f}" cy="{Y(r[idx]):.1f}" r="4.5" fill="{col}" '
                     f'stroke="{t["surface"]}" stroke-width="2"/>')
        last = LOC[-1]
        b.append(text(X(last[0]) + 14, Y(last[idx]) - 4, name, 14, t["ink"], 650))
        b.append(text(X(last[0]) + 14, Y(last[idx]) + 14, f"{last[idx]:,} tests", 13, t["ink2"]))
    # callout at 64 nodes
    n, g, p = LOC[2]
    b.append(line(X(n), Y(p) + 8, X(n), Y(g) - 8, t["axis"], 1, ' stroke-dasharray="3 3"'))
    b.append(text(X(n) + 10, (Y(p) + Y(g)) / 2 - 6, "64 nodes:", 13, t["ink"], 650))
    b.append(text(X(n) + 10, (Y(p) + Y(g)) / 2 + 12, "23 tests instead of 2,016", 13, t["ink2"]))
    return svg(W, H, t, b, "Finding 2 bad nodes without testing every pair",
               "Group tests needed to find 2 bad nodes vs all node pairs: 16 nodes 15 vs 120; 32 nodes 19 vs 496; "
               "64 nodes 23 vs 2,016; 128 nodes 27 vs 8,128; 256 nodes 31 vs 32,640; 512 nodes 35 vs 130,816.")


def main():
    out = sys.argv[1]
    os.makedirs(out, exist_ok=True)
    for name, fn in (("banner", banner), ("sla-statement", terminal), ("sla-month", sla_month),
                     ("corroboration", corroboration), ("fault-localisation", localisation)):
        for mode, t in THEMES.items():
            with open(os.path.join(out, f"{name}-{mode}.svg"), "w", encoding="utf-8", newline="\n") as f:
                f.write(fn(t))
    print("wrote images to", out)


if __name__ == "__main__":
    main()

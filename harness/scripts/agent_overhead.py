#!/usr/bin/env python3
"""Measure a running process's CPU and memory use, for the proof-of-value
threshold "passive agent under 1% of one CPU core and 200 MB RAM"
(docs/roadmap.md).

    python3 agent_overhead.py --pid "$(pidof capledger)" --seconds 3600

Linux only: it reads /proc/PID/stat and /proc/PID/status at the start and
the end of the window and every --interval seconds in between (for the peak
resident memory). Prints one JSON object and exits 1 if a threshold is
exceeded.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time

CPU_LIMIT_PERCENT = 1.0
RSS_LIMIT_MB = 200.0


def cpu_seconds(pid: int) -> float:
    with open(f"/proc/{pid}/stat", encoding="ascii") as f:
        stat = f.read()
    # The command name may contain spaces and parentheses; fields follow the last ')'.
    fields = stat[stat.rindex(")") + 2:].split()
    utime, stime = int(fields[11]), int(fields[12])
    return (utime + stime) / os.sysconf("SC_CLK_TCK")


def rss_mb(pid: int) -> float:
    with open(f"/proc/{pid}/status", encoding="ascii") as f:
        for line in f:
            if line.startswith("VmRSS:"):
                return int(line.split()[1]) / 1024.0
    return 0.0


def measure(pid: int, seconds: float, interval: float) -> dict:
    t0, c0 = time.monotonic(), cpu_seconds(pid)
    peak = rss_mb(pid)
    deadline = t0 + seconds
    while True:
        now = time.monotonic()
        if now >= deadline:
            break
        time.sleep(min(interval, deadline - now))
        peak = max(peak, rss_mb(pid))
    t1, c1 = time.monotonic(), cpu_seconds(pid)
    cpu = 100.0 * (c1 - c0) / (t1 - t0)
    return {
        "pid": pid,
        "seconds": round(t1 - t0, 1),
        "cpuPercentOfOneCore": round(cpu, 3),
        "peakRssMB": round(peak, 1),
        "cpuLimitPercent": CPU_LIMIT_PERCENT,
        "rssLimitMB": RSS_LIMIT_MB,
        "pass": cpu < CPU_LIMIT_PERCENT and peak < RSS_LIMIT_MB,
    }


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("--pid", type=int, required=True)
    p.add_argument("--seconds", type=float, default=600, help="measurement window (default 600)")
    p.add_argument("--interval", type=float, default=5, help="memory sampling interval (default 5)")
    a = p.parse_args(argv)
    if not os.path.exists(f"/proc/{a.pid}/stat"):
        print(f"no process {a.pid} (this script reads Linux /proc)", file=sys.stderr)
        return 2
    try:
        res = measure(a.pid, a.seconds, a.interval)
    except FileNotFoundError:
        print(f"process {a.pid} exited during the measurement", file=sys.stderr)
        return 2
    print(json.dumps(res, indent=2))
    return 0 if res["pass"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

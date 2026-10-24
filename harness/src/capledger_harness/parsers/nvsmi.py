"""Parse ``nvidia-smi --query-gpu=... --format=csv,noheader,nounits`` output."""

from __future__ import annotations


def parse_csv(text: str, fields: list[str]) -> list[dict[str, str]]:
    """Return one dict per GPU line, keyed by the queried field names."""
    rows = []
    for line in text.splitlines():
        if not line.strip():
            continue
        values = [v.strip() for v in line.split(",")]
        if len(values) != len(fields):
            raise ValueError(f"expected {len(fields)} fields, got {len(values)}: {line!r}")
        rows.append(dict(zip(fields, values)))
    return rows


def count_gpus(text: str) -> int:
    """Count GPU lines in nvidia-smi CSV output (one GPU per line).

    Lines that do not look like a GPU record (for example error messages)
    are ignored: a record starts with the integer GPU index. Counting stops
    at the first ``--- `` separator of an audit, so later sections (topology,
    RDMA) cannot add to the count.
    """
    n = 0
    for line in text.splitlines():
        if line.startswith("--- "):
            break
        head = line.split(",", 1)[0].strip()
        if head.isdigit():
            n += 1
    return n

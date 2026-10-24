"""Parse the output of AMD Instinct acceptance commands."""

from __future__ import annotations

import re

PCI_ADDRESS = re.compile(r"^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$")
# ROCm Validation Suite result lines end in "pass: FALSE" or "met: FALSE"
# when a check fails. The format differs between releases, so the exit code
# is checked as well and the raw output is kept for review.
RVS_FAILURE = re.compile(r"\b(?:pass|met)\s*:\s*FALSE\b", re.IGNORECASE)


def count_amd_gpus(text: str) -> int:
    """Count amdgpu PCI functions in the first section of an AMD audit.

    The audit lists one PCI address per line (from
    /sys/bus/pci/drivers/amdgpu) before its first ``--- `` separator.
    Partitioned GPUs can expose more functions than physical GPUs; set the
    expected count to what a healthy node shows.
    """
    n = 0
    for line in text.splitlines():
        if line.startswith("--- "):
            break
        if PCI_ADDRESS.match(line.strip()):
            n += 1
    return n


def rvs_failed(text: str) -> bool:
    """True if ROCm Validation Suite output reports a failed check."""
    return bool(RVS_FAILURE.search(text))

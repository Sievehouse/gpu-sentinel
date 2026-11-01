"""GPU Sentinel acceptance harness.

Plans and runs cluster acceptance tests with proven open-source tools
(nccl-tests, DCGM diagnostics, perftest, fio), parses their output, flags
slow nodes against their peers and localises faulty nodes with group testing.
"""

__version__ = "0.5.0"

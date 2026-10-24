"""Short GEMM benchmark for the tenant probe (reference implementation).

Runs square matrix multiplications on every visible GPU and prints the
slowest GPU's TFLOPS as the last line, which is what `capledger probe
--bench-cmd` reads:

    capledger probe --expected-gpus 8 \
        --bench-cmd "python3 harness/scripts/gemm_bench.py --seconds 20" \
        --bench-floor 600 ...

Reporting the slowest GPU means one throttled or degraded GPU pulls the
number below the floor. Calibrate the floor from healthy nodes of the same
GPU model; do not copy a vendor peak figure.

Status: reference code, not yet validated on hardware in this repository.
"""

from __future__ import annotations

import argparse
import sys
import time


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("--size", type=int, default=8192, help="matrix size n for n x n GEMMs")
    p.add_argument("--seconds", type=float, default=20.0, help="timed run per GPU")
    p.add_argument("--dtype", choices=("bf16", "fp16", "fp32"), default="bf16")
    args = p.parse_args()

    try:
        import torch
    except ImportError:
        print("PyTorch is not installed", file=sys.stderr)
        return 2
    if not torch.cuda.is_available():  # also true for ROCm builds
        print("no GPU visible to PyTorch", file=sys.stderr)
        return 2

    dtype = {"bf16": torch.bfloat16, "fp16": torch.float16, "fp32": torch.float32}[args.dtype]
    n = args.size
    results = []
    for dev in range(torch.cuda.device_count()):
        torch.cuda.set_device(dev)
        a = torch.randn(n, n, device=dev, dtype=dtype)
        b = torch.randn(n, n, device=dev, dtype=dtype)
        for _ in range(5):  # warm-up
            torch.mm(a, b)
        torch.cuda.synchronize(dev)
        iters, t0 = 0, time.perf_counter()
        while time.perf_counter() - t0 < args.seconds:
            for _ in range(10):
                torch.mm(a, b)
            torch.cuda.synchronize(dev)
            iters += 10
        elapsed = time.perf_counter() - t0
        tflops = 2 * n**3 * iters / elapsed / 1e12
        results.append(tflops)
        print(f"gpu {dev}: {tflops:.1f} TFLOPS", file=sys.stderr)
    print(f"{min(results):.2f}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

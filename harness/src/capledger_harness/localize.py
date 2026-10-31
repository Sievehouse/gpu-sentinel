"""Find faulty nodes with adaptive group testing.

A multi-node collective is slow if any node in it is slow. Instead of testing
every node pair, test the whole cluster, split failing groups in half and
recurse. With k bad nodes among n this needs about 2·k·log2(n/k) tests, and
tests on disjoint groups at the same depth can run in the same round.

A larger fan-out splits a failing group into more parts per round: more
tests, fewer rounds. With fan-out 8, two bad nodes among 64 are found in 3
rounds (whole cluster, eight groups of eight, the suspects one by one)
instead of 7.

Collectives need at least two nodes, so a single suspect node is tested
together with a node already known to be healthy.

Caveat: concurrent collectives on disjoint groups still share the fabric.
If the fabric is oversubscribed, run each round's tests one after another;
``tests`` then equals the number of sequential runs.
"""

from __future__ import annotations

import random
import statistics
from dataclasses import dataclass, field
from typing import Callable, Sequence

Group = tuple[str, ...]
TestFn = Callable[[Group], bool]  # True means "healthy"


@dataclass(frozen=True)
class LocalizeResult:
    bad: tuple[str, ...]
    tests: int
    rounds: int
    unresolved: tuple[str, ...]
    log: tuple[tuple[Group, bool], ...] = field(repr=False)


def split(g: Group, fanout: int) -> list[Group]:
    """Split g into min(fanout, len(g)) near-equal parts, larger ones last
    (for fan-out 2: g[:len//2] and g[len//2:])."""
    k = min(fanout, len(g))
    size, extra = divmod(len(g), k)
    parts, i = [], 0
    for j in range(k):
        n = size + (1 if j >= k - extra else 0)
        parts.append(g[i:i + n])
        i += n
    return parts


def localize(nodes: Sequence[str], test: TestFn, fanout: int = 2) -> LocalizeResult:
    """Return the nodes for which every group containing them failed."""
    if len(nodes) < 2:
        raise ValueError("need at least two nodes")
    if fanout < 2:
        raise ValueError("fan-out must be at least 2")
    known_good: list[str] = []
    bad: list[str] = []
    log: list[tuple[Group, bool]] = []
    frontier: list[Group] = [tuple(nodes)]
    rounds = 0
    while frontier:
        rounds += 1
        deferred: list[Group] = []
        results: list[tuple[Group, bool]] = []
        for g in frontier:
            if len(g) == 1:
                if not known_good:
                    deferred.append(g)
                    continue
                probe = (g[0], known_good[0])
            else:
                probe = g
            ok = test(probe)
            log.append((probe, ok))
            results.append((g, ok))
        progressed = False
        nxt: list[Group] = []
        for g, ok in results:
            progressed = True
            if ok:
                known_good.extend(g)
            elif len(g) == 1:
                bad.append(g[0])
            else:
                nxt.extend(split(g, fanout))
        if deferred and not progressed and not known_good:
            # Every remaining group is a singleton and no healthy reference
            # exists yet: report them as unresolved rather than guessing.
            return LocalizeResult(tuple(sorted(bad)), len(log), rounds, tuple(sorted(n for g in deferred for n in g)), tuple(log))
        frontier = nxt + deferred
    return LocalizeResult(tuple(sorted(bad)), len(log), rounds, (), tuple(log))


@dataclass
class SimulatedCluster:
    """A toy fabric model for exercising the algorithm without GPUs.

    Each node has a speed factor (1.0 = healthy). A collective runs at the
    speed of its slowest member, times Gaussian measurement noise.
    """

    factors: dict[str, float]
    noise: float = 0.015
    base_busbw: float = 360.0
    seed: int = 0
    measurements: int = 0
    _rng: random.Random = field(init=False, repr=False)

    def __post_init__(self) -> None:
        self._rng = random.Random(self.seed)

    def measure(self, group: Group) -> float:
        self.measurements += 1
        slowest = min(self.factors[n] for n in group)
        return self.base_busbw * slowest * (1.0 + self._rng.gauss(0.0, self.noise))


def threshold_test(cluster: SimulatedCluster, *, floor_ratio: float = 0.92, margin: float = 0.02, max_repeats: int = 5) -> TestFn:
    """Turn measurements into a pass/fail test.

    A group passes if its bus bandwidth is at least ``floor_ratio`` of the
    healthy baseline. Results within ``margin`` of the floor are re-measured
    (up to ``max_repeats`` times) and judged on the median, which keeps noise
    from flipping verdicts. Real clusters need a baseline per group size.
    """
    floor = cluster.base_busbw * floor_ratio

    def test(group: Group) -> bool:
        values = [cluster.measure(group)]
        while len(values) < max_repeats and abs(statistics.median(values) - floor) / floor < margin:
            values.append(cluster.measure(group))
        return statistics.median(values) >= floor

    return test


def simulate(n_nodes: int = 64, n_bad: int = 2, degradation: float = 0.20, noise: float = 0.015, seed: int = 7, fanout: int = 2) -> dict:
    """Run one simulated localisation and compare it with the ground truth."""
    rng = random.Random(seed)
    nodes = [f"gpu-{i:03d}" for i in range(1, n_nodes + 1)]
    truth = sorted(rng.sample(nodes, n_bad))
    factors = {n: (1.0 - degradation if n in truth else 1.0) for n in nodes}
    cluster = SimulatedCluster(factors=factors, noise=noise, seed=seed)
    result = localize(nodes, threshold_test(cluster), fanout)
    return {
        "nodes": n_nodes,
        "injected_bad": truth,
        "found_bad": list(result.bad),
        "unresolved": list(result.unresolved),
        "correct": list(result.bad) == truth and not result.unresolved,
        "logical_tests": result.tests,
        "measurements": cluster.measurements,
        "rounds": result.rounds,
        "fanout": fanout,
        "all_pairs_tests_for_comparison": n_nodes * (n_nodes - 1) // 2,
    }

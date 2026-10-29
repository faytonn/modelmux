from __future__ import annotations

import math
from collections import defaultdict, deque


class LatencyTracker:
    """Keeps the last N successful call latencies per provider and computes p95
    over them. A fixed-size window means old samples age out, so the latency
    policy reacts when a provider gets slower or recovers."""

    def __init__(self, window: int) -> None:
        # deque(maxlen=...) drops the oldest sample when a new one arrives.
        self._samples: defaultdict[str, deque[float]] = defaultdict(lambda: deque(maxlen=window))

    def observe(self, provider: str, seconds: float) -> None:
        self._samples[provider].append(seconds)

    def p95(self, provider: str) -> float | None:
        """The nearest-rank 95th percentile in seconds, or None with no samples."""
        samples = self._samples.get(provider)
        if not samples:
            return None
        ordered = sorted(samples)
        return ordered[math.ceil(0.95 * len(ordered)) - 1]

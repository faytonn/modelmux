from __future__ import annotations

import asyncio
import random
from collections.abc import Awaitable, Callable
from dataclasses import dataclass


@dataclass
class RetryPolicy:
    max_attempts: int = 3  # total attempts, including the first
    base_delay: float = 0.1  # seconds
    max_delay: float = 2.0
    # If the server asks us to wait longer than this, give up so the caller can fall back.
    max_retry_after: float = 0.0

    # Reports whether an error is worth retrying and any delay the server asked
    # for (e.g. Retry-After). If None, nothing is retried.
    classify: Callable[[Exception], tuple[bool, float]] | None = None
    # Called before each wait with (attempt, error, delay). Used for logging and metrics.
    on_retry: Callable[[int, Exception, float], None] | None = None

    # Test hooks.
    rand: Callable[[], float] = random.random
    sleep: Callable[[float], Awaitable[None]] = asyncio.sleep

    def backoff(self, attempt: int) -> float:
        """The wait before retrying after the given attempt (1-based), using
        "full jitter": a random duration in [0, min(max_delay, base_delay * 2^(attempt-1))).
        Jitter spreads retries out so that many clients failing at the same
        moment do not all retry at the same moment too."""
        ceiling = min(self.max_delay, self.base_delay * 2 ** min(attempt - 1, 32))
        return self.rand() * ceiling


async def retry[T](
    fn: Callable[[], Awaitable[T]], policy: RetryPolicy, *, deadline: float | None = None
) -> T:
    """Calls fn until it succeeds, raises a non-retryable error, runs out of
    attempts, or the task is cancelled.

    deadline is the request's deadline on the event loop clock (loop.time()).
    retry never sleeps past it: if the next wait would not fit, it re-raises the
    last error straight away so the caller can spend the remaining time on a
    fallback instead.
    """
    loop = asyncio.get_running_loop()
    attempt = 1
    while True:
        try:
            return await fn()
        except Exception as err:
            if attempt >= policy.max_attempts or policy.classify is None:
                raise
            retryable, retry_after = policy.classify(err)
            if not retryable:
                raise
            delay = policy.backoff(attempt)
            if retry_after > 0:
                if policy.max_retry_after > 0 and retry_after > policy.max_retry_after:
                    raise
                delay = max(delay, retry_after)  # the server's minimum wins
            if deadline is not None and deadline - loop.time() < delay:
                raise
            if policy.on_retry is not None:
                policy.on_retry(attempt, err, delay)
        # Sleep outside the except block. Cancellation during the wait (client
        # gone, request budget spent) propagates as CancelledError.
        await policy.sleep(delay)
        attempt += 1

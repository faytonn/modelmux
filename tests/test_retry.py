import asyncio

import pytest

from modelmux.resilience import RetryPolicy, retry


class Transient(Exception):
    pass


class Permanent(Exception):
    pass


class SlowDown(Exception):
    pass


def classify(err: Exception) -> tuple[bool, float]:
    if isinstance(err, Transient):
        return True, 0.0
    if isinstance(err, SlowDown):
        return True, 0.5
    return False, 0.0


def policy(waits: list[float], **overrides) -> RetryPolicy:
    """Records every wait instead of sleeping, so tests run instantly."""

    async def sleep(d: float) -> None:
        waits.append(d)

    p = RetryPolicy(
        max_attempts=3, base_delay=0.1, max_delay=1.0, classify=classify, rand=lambda: 1.0, sleep=sleep
    )
    for k, v in overrides.items():
        setattr(p, k, v)
    return p


def failing(*errors: Exception):
    """fn that raises errors in order, then returns "ok". calls[0] counts calls."""
    calls = [0]

    async def fn() -> str:
        calls[0] += 1
        if calls[0] <= len(errors):
            raise errors[calls[0] - 1]
        return "ok"

    return fn, calls


async def test_retries_transient_errors_then_succeeds() -> None:
    waits: list[float] = []
    fn, calls = failing(Transient(), Transient())
    assert await retry(fn, policy(waits)) == "ok"
    assert calls[0] == 3
    assert waits == pytest.approx([0.1, 0.2])  # exponential


async def test_stops_at_max_attempts() -> None:
    fn, calls = failing(*[Transient()] * 4)
    with pytest.raises(Transient):
        await retry(fn, policy([]))
    assert calls[0] == 3


async def test_does_not_retry_permanent_errors() -> None:
    fn, calls = failing(Permanent())
    with pytest.raises(Permanent):
        await retry(fn, policy([]))
    assert calls[0] == 1


async def test_honours_retry_after() -> None:
    waits: list[float] = []
    fn, _ = failing(SlowDown())
    await retry(fn, policy(waits))
    assert waits == [0.5]  # backoff alone would be 0.1s; the server asked for 0.5s


async def test_gives_up_when_retry_after_too_long() -> None:
    waits: list[float] = []
    fn, calls = failing(SlowDown())
    with pytest.raises(SlowDown):
        await retry(fn, policy(waits, max_retry_after=0.2))
    assert (calls[0], waits) == (1, [])


async def test_will_not_wait_past_deadline() -> None:
    waits: list[float] = []
    fn, calls = failing(Transient())
    deadline = asyncio.get_running_loop().time() + 0.05  # less than the 0.1s backoff
    with pytest.raises(Transient):
        await retry(fn, policy(waits), deadline=deadline)
    assert (calls[0], waits) == (1, [])


async def test_cancellation_during_wait_stops_retrying() -> None:
    fn, calls = failing(Transient(), Transient())
    p = RetryPolicy(max_attempts=5, base_delay=3600, max_delay=3600, classify=classify, rand=lambda: 1.0)
    task = asyncio.create_task(retry(fn, p))
    await asyncio.sleep(0.02)
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    assert calls[0] == 1


@pytest.mark.parametrize(
    ("attempt", "want"), [(1, 0.1), (2, 0.2), (3, 0.4), (4, 0.8), (5, 1.0), (60, 1.0)]
)  # capped at max_delay
def test_backoff_is_capped(attempt: int, want: float) -> None:
    p = RetryPolicy(base_delay=0.1, max_delay=1.0, rand=lambda: 1.0)
    assert p.backoff(attempt) == pytest.approx(want)


def test_backoff_is_jittered() -> None:
    p = RetryPolicy(base_delay=0.1, max_delay=1.0, rand=lambda: 0.25)
    assert p.backoff(3) == pytest.approx(0.1)  # a quarter of 0.4s

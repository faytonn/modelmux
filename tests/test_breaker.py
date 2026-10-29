from concurrent.futures import ThreadPoolExecutor
from threading import Barrier

from modelmux.resilience import Breaker, State


class FakeClock:
    """Lets tests move time forward instead of sleeping."""

    def __init__(self) -> None:
        self.t = 0.0

    def __call__(self) -> float:
        return self.t


def new_breaker() -> tuple[Breaker, FakeClock, list[str]]:
    clock, transitions = FakeClock(), []
    b = Breaker(3, 10.0, lambda a, b: transitions.append(f"{a}->{b}"), now=clock)
    return b, clock, transitions


def fail(b: Breaker, n: int) -> None:
    for _ in range(n):
        if b.allow():
            b.failure()


def test_opens_after_consecutive_failures() -> None:
    b, _, _ = new_breaker()
    fail(b, 2)
    assert b.state == State.CLOSED
    fail(b, 1)
    assert b.state == State.OPEN
    assert not b.allow()


def test_success_resets_failure_count() -> None:
    b, _, _ = new_breaker()
    fail(b, 2)
    b.allow()
    b.success()
    fail(b, 2)
    assert b.state == State.CLOSED  # the failures were not consecutive


def test_half_open_probe_success_closes() -> None:
    b, clock, transitions = new_breaker()
    fail(b, 3)

    clock.t += 9
    assert not b.allow(), "allowed a call before the open timeout passed"
    clock.t += 1
    assert b.allow(), "did not allow a probe after the open timeout"
    assert not b.allow(), "allowed a second call while the probe is in flight"
    b.success()

    assert b.state == State.CLOSED
    assert b.allow()
    assert transitions == ["closed->open", "open->half-open", "half-open->closed"]


def test_half_open_probe_failure_reopens() -> None:
    b, clock, _ = new_breaker()
    fail(b, 3)
    clock.t += 10
    b.allow()
    b.failure()
    assert b.state == State.OPEN
    clock.t += 5
    assert not b.allow(), "the open timeout should restart from the failed probe"


def test_release_frees_probe_slot() -> None:
    b, clock, _ = new_breaker()
    fail(b, 3)
    clock.t += 10
    b.allow()
    b.release()  # e.g. the client hung up mid-probe
    assert b.state == State.HALF_OPEN
    assert b.allow()


def test_allows_exactly_one_concurrent_probe() -> None:
    b, clock, _ = new_breaker()
    fail(b, 3)
    clock.t += 10

    # 100 threads all call allow() at the same moment.
    start = Barrier(100)

    def try_allow(_: int) -> bool:
        start.wait()
        return b.allow()

    with ThreadPoolExecutor(max_workers=100) as pool:
        allowed = sum(pool.map(try_allow, range(100)))
    assert allowed == 1

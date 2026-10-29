import asyncio
import time

import pytest
from prometheus_client import CollectorRegistry

from helpers import HELLO, attempt_keys, make_router, mock_cand
from modelmux.metrics import Metrics
from modelmux.resilience import State
from modelmux.router import AllFailedError, BudgetExhaustedError


async def test_falls_back_when_primary_keeps_failing() -> None:
    r = make_router(mock_cand("a", 0.001, error_rate=1), mock_cand("b", 0.002))
    res = await r.chat(HELLO)
    assert res.provider == "b"
    assert attempt_keys(res) == "a:err,a:err,b:ok"  # a is retried once (max_attempts 2), then b succeeds


async def test_attempt_timeout_triggers_fallback() -> None:
    r = make_router(mock_cand("slow", 0.001, timeout=0.02, latency=1), mock_cand("b", 0.002))
    start = time.perf_counter()
    res = await r.chat(HELLO)
    assert res.provider == "b"
    assert "attempt timed out" in res.attempts[0].error
    # The 1s mock latency was cut off by the 20ms attempt timeout.
    assert time.perf_counter() - start < 0.5


async def test_long_retry_after_falls_back_without_waiting() -> None:
    r = make_router(mock_cand("limited", 0.001, rate_limit_rate=1, retry_after=10), mock_cand("b", 0.002))
    res = await r.chat(HELLO)
    # Retry-After (10s) exceeds max_retry_after (1s), so "limited" is not retried.
    assert attempt_keys(res) == "limited:err,b:ok"


async def test_all_candidates_fail() -> None:
    r = make_router(mock_cand("a", 0.001, error_rate=1), mock_cand("b", 0.002, error_rate=1))
    with pytest.raises(AllFailedError) as exc:
        await r.chat(HELLO)
    assert len(exc.value.result.attempts) == 4  # 2 per candidate


async def test_request_budget_stops_the_chain() -> None:
    r = make_router(mock_cand("slow", 0.001, latency=1), mock_cand("b", 0.002))
    with pytest.raises(BudgetExhaustedError) as exc:
        await r.chat(HELLO, timeout=0.03)
    assert attempt_keys(exc.value.result) == "slow:err"
    assert exc.value.result.attempts[0].error == "request budget exhausted"


async def test_client_cancel_stops_the_chain_and_frees_the_breaker() -> None:
    r = make_router(mock_cand("slow", 0.001, latency=1), mock_cand("b", 0.002))
    task = asyncio.create_task(r.chat(HELLO))
    await asyncio.sleep(0.02)
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    # A cancelled call says nothing about the provider's health.
    assert r.breakers["slow"].state == State.CLOSED


async def test_open_breaker_skips_provider_without_calling_it() -> None:
    r = make_router(mock_cand("a", 0.001, error_rate=1), mock_cand("b", 0.002), threshold=2)

    # First request: both attempts on a fail, which opens a's breaker; b serves.
    res = await r.chat(HELLO)
    assert attempt_keys(res) == "a:err,a:err,b:ok"
    assert r.breakers["a"].state == State.OPEN

    # Second request: a is skipped instantly, no retries, straight to b.
    res = await r.chat(HELLO)
    assert attempt_keys(res) == "a:err,b:ok"
    assert res.attempts[0].error == "a: circuit breaker open"


async def test_router_records_metrics() -> None:
    reg = CollectorRegistry()
    r = make_router(mock_cand("a", 0.001, error_rate=1), mock_cand("b", 0.002), metrics=Metrics(reg))
    await r.chat(HELLO)

    def value(name: str, **labels: str) -> float | None:
        return reg.get_sample_value(name, labels)

    assert value("modelmux_fallbacks_total", provider="a", model="m") == 1
    assert value("modelmux_retries_total", provider="a", model="m") == 1
    assert value("modelmux_upstream_requests_total", provider="a", model="m", outcome="error") == 2
    assert value("modelmux_upstream_requests_total", provider="b", model="m", outcome="success") == 1
    assert value("modelmux_circuit_breaker_state", provider="a") == 0

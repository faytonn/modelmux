from __future__ import annotations

from modelmux.config import BreakerConfig, ModelConfig, RetryConfig, RoutingConfig
from modelmux.metrics import Metrics
from modelmux.providers import Message, Mock, MockConfig
from modelmux.router import Candidate, ChatRequest, Result, Router

HELLO = ChatRequest(messages=[Message("user", "hello")], max_tokens=50)


def cand(
    provider: str, model: str, context_window: int, in_price: float, out_price: float, quality: int
) -> Candidate:
    return Candidate(
        Mock(provider),
        ModelConfig(
            name=model,
            context_window=context_window,
            input_price_per_1k=in_price,
            output_price_per_1k=out_price,
            quality=quality,
        ),
    )


def mock_cand(name: str, price: float, timeout: float = 0.0, **mock: float) -> Candidate:
    """A candidate backed by a mock provider. A lower price means the cost
    policy tries it earlier."""
    return Candidate(
        Mock(name, MockConfig(**mock)),
        ModelConfig(name="m", context_window=100_000, input_price_per_1k=price, output_price_per_1k=price),
        timeout,
    )


def make_router(
    *cands: Candidate, threshold: int = 100, metrics: Metrics | None = None, policy: str = "cost"
) -> Router:
    return Router(
        cands,
        RoutingConfig(policy=policy, latency_window=10),
        RetryConfig(max_attempts=2, base_delay=0.001, max_delay=0.005, max_retry_after=1.0),
        BreakerConfig(failure_threshold=threshold, open_timeout=60),
        metrics=metrics,
    )


def attempt_keys(res: Result) -> str:
    return ",".join(f"{a.provider}:{'err' if a.error else 'ok'}" for a in res.attempts)

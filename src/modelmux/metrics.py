"""The gateway's Prometheus metrics.

Labels are limited to values that come from config (provider, model) or a
small fixed set (outcome, code), so label cardinality stays bounded no matter
what clients send.
"""

from __future__ import annotations

from prometheus_client import (
    CollectorRegistry,
    Counter,
    Gauge,
    GCCollector,
    Histogram,
    PlatformCollector,
    ProcessCollector,
    disable_created_metrics,
)

# prometheus_client adds a *_created series next to every counter by default.
# Nothing here queries them, and they double the size of /metrics.
disable_created_metrics()

# Attempt outcomes, used as the "outcome" label.
OUTCOME_SUCCESS = "success"
OUTCOME_ERROR = "error"
OUTCOME_RATE_LIMITED = "rate_limited"
OUTCOME_TIMEOUT = "timeout"
OUTCOME_BREAKER_OPEN = "breaker_open"
OUTCOME_CANCELED = "canceled"


class Metrics:
    """Every collector, registered on a dedicated registry instead of the
    global default. That keeps metrics explicit and lets tests build as many
    gateways as they like."""

    def __init__(self, registry: CollectorRegistry | None = None) -> None:
        self.registry = registry if registry is not None else CollectorRegistry()
        r = self.registry
        self.attempts = Counter(
            "modelmux_upstream_requests",
            "Upstream provider calls by outcome.",
            ["provider", "model", "outcome"],
            registry=r,
        )
        self.latency = Histogram(
            "modelmux_upstream_request_duration_seconds",
            "Upstream provider call latency.",
            ["provider", "model"],
            buckets=(0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60),
            registry=r,
        )
        self.retries = Counter(
            "modelmux_retries", "Retries scheduled against a provider.", ["provider", "model"], registry=r
        )
        self.fallbacks = Counter(
            "modelmux_fallbacks",
            "Times the router gave up on this provider/model and moved to the next candidate.",
            ["provider", "model"],
            registry=r,
        )
        self.cost = Counter(
            "modelmux_estimated_cost_usd",
            "Estimated spend from configured per-1K-token prices.",
            ["provider", "model"],
            registry=r,
        )
        self.tokens = Counter(
            "modelmux_tokens",
            "Tokens processed, by direction (input or output).",
            ["provider", "model", "direction"],
            registry=r,
        )
        self.breaker_state = Gauge(
            "modelmux_circuit_breaker_state",
            "Circuit breaker state per provider: 0 closed, 1 open, 2 half-open.",
            ["provider"],
            registry=r,
        )
        self.http_requests = Counter(
            "modelmux_http_requests", "Requests to POST /v1/chat by HTTP status code.", ["code"], registry=r
        )
        self.http_duration = Histogram(
            "modelmux_http_request_duration_seconds",
            "End-to-end latency of POST /v1/chat, including retries and fallbacks.",
            ["code"],
            buckets=(0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60),
            registry=r,
        )
        # Python runtime and process metrics, the equivalent of the Go collectors.
        ProcessCollector(registry=r)
        PlatformCollector(registry=r)
        GCCollector(registry=r)

    def observe_attempt(self, provider: str, model: str, outcome: str, seconds: float) -> None:
        """Records one upstream call. Calls rejected by an open breaker never
        reached the provider, so they are counted but not timed."""
        self.attempts.labels(provider, model, outcome).inc()
        if outcome != OUTCOME_BREAKER_OPEN:
            self.latency.labels(provider, model).observe(seconds)

    def observe_retry(self, provider: str, model: str) -> None:
        self.retries.labels(provider, model).inc()

    def observe_fallback(self, provider: str, model: str) -> None:
        self.fallbacks.labels(provider, model).inc()

    def observe_usage(
        self, provider: str, model: str, input_tokens: int, output_tokens: int, cost_usd: float
    ) -> None:
        """Records tokens and estimated cost for a successful call."""
        self.tokens.labels(provider, model, "input").inc(input_tokens)
        self.tokens.labels(provider, model, "output").inc(output_tokens)
        self.cost.labels(provider, model).inc(cost_usd)

    def set_breaker_state(self, provider: str, state: int) -> None:
        """0 closed, 1 open, 2 half-open."""
        self.breaker_state.labels(provider).set(state)

    def observe_http(self, code: int, seconds: float) -> None:
        self.http_requests.labels(str(code)).inc()
        self.http_duration.labels(str(code)).observe(seconds)

"""Decides which provider and model serve a request, and walks the fallback
chain with retries when calls fail."""

from __future__ import annotations

import asyncio
import dataclasses
import logging
import time
from collections.abc import Sequence
from dataclasses import dataclass, field
from typing import Any

from .. import metrics as m
from ..config import BreakerConfig, ModelConfig, RetryConfig, RoutingConfig
from ..providers import (
    AttemptTimeoutError,
    Message,
    Provider,
    ProviderError,
    Request,
    Response,
    classify,
    describe,
    estimate_tokens,
)
from ..resilience import Breaker, BreakerOpenError, RetryPolicy, State, retry
from .latency import LatencyTracker
from .policy import CostPolicy, LatencyPolicy, Policy, QualityPolicy, RouteInfo, RulePolicy, fitting


@dataclass(frozen=True)
class Candidate:
    """One (provider, model) pair the router can send a request to."""

    provider: Provider
    model: ModelConfig
    timeout: float = 0.0  # per-attempt timeout in seconds; 0 means only the request budget applies

    @property
    def key(self) -> str:
        """Identifies the candidate as "provider/model"."""
        return f"{self.provider.name}/{self.model.name}"


@dataclass(frozen=True)
class ChatRequest:
    """What the router receives from the HTTP layer."""

    messages: list[Message]
    max_tokens: int
    hint: str = ""  # optional: "cheap", "fast" or "quality"


@dataclass
class Attempt:
    """One upstream call, for the response and for debugging."""

    provider: str
    model: str
    duration_ms: int = 0
    error: str | None = None

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {"provider": self.provider, "model": self.model, "duration_ms": self.duration_ms}
        if self.error is not None:
            d["error"] = self.error
        return d


@dataclass
class Result:
    """The outcome of a routed request. On failure it still carries the
    attempts that were made."""

    policy: str
    provider: str = ""
    model: str = ""
    response: Response | None = None
    cost_usd: float = 0.0
    attempts: list[Attempt] = field(default_factory=list)


class RouterError(Exception):
    def __init__(self, message: str, result: Result | None = None) -> None:
        super().__init__(message)
        self.result = result


class NoCandidatesError(RouterError):
    """No configured model can serve the request."""


class AllFailedError(RouterError):
    """Every candidate in the fallback chain failed."""


class BudgetExhaustedError(RouterError):
    """The request timeout ran out before any candidate succeeded."""


# A request's routing_hint mapped to a policy name.
HINT_POLICIES = {"cheap": "cost", "fast": "latency", "quality": "quality"}


class Router:
    def __init__(
        self,
        candidates: Sequence[Candidate],
        routing: RoutingConfig,
        retry: RetryConfig,
        breaker: BreakerConfig,
        metrics: m.Metrics | None = None,
        log: logging.Logger | None = None,
    ) -> None:
        self.candidates = list(candidates)
        # Without a metrics object the router records into a private registry
        # that nobody scrapes, so the code below never has to check for None.
        self.metrics = metrics or m.Metrics()
        self.log = log or logging.getLogger("modelmux.router")
        self.latency = LatencyTracker(routing.latency_window)
        self.policies: dict[str, Policy] = {
            "cost": CostPolicy(),
            "latency": LatencyPolicy(self.latency),
            "quality": QualityPolicy(),
            "rule": RulePolicy(routing.rules, base=CostPolicy()),
        }
        if routing.policy not in self.policies:
            raise ValueError(f"unknown routing policy {routing.policy!r}")
        self.default_policy = self.policies[routing.policy]
        self.retry_policy = RetryPolicy(
            max_attempts=retry.max_attempts,
            base_delay=retry.base_delay,
            max_delay=retry.max_delay,
            max_retry_after=retry.max_retry_after,
            classify=classify,
        )
        # One breaker per provider: models of the same provider share it.
        self.breakers: dict[str, Breaker] = {}
        for c in self.candidates:
            name = c.provider.name
            if name not in self.breakers:
                self.breakers[name] = Breaker(
                    breaker.failure_threshold, breaker.open_timeout, self._on_breaker_change(name)
                )
                self.metrics.set_breaker_state(name, State.CLOSED)

    def _on_breaker_change(self, provider: str):
        def on_change(before: State, after: State) -> None:
            self.metrics.set_breaker_state(provider, after)
            self.log.warning(
                "circuit breaker state change",
                extra={"provider": provider, "from": str(before), "to": str(after)},
            )

        return on_change

    def plan(self, req: ChatRequest) -> tuple[Policy, list[Candidate]]:
        """Picks the policy for this request and returns the ordered candidates."""
        policy = self.policies[HINT_POLICIES[req.hint]] if req.hint in HINT_POLICIES else self.default_policy
        info = RouteInfo(prompt_tokens=estimate_tokens(req.messages), max_tokens=req.max_tokens)
        return policy, policy.rank(info, fitting(info, self.candidates))

    async def chat(self, req: ChatRequest, *, timeout: float | None = None) -> Result:
        """Walks the ranked candidates in order. Each candidate gets its own
        retries; if it still fails, the next one is tried. Any provider error
        moves on to the next candidate (a 401 or a model-specific 400 at one
        provider says nothing about another).

        timeout bounds the whole request, retries and fallbacks included. If
        the calling task is cancelled (the client disconnected), CancelledError
        propagates and every in-flight upstream call is cancelled with it.
        """
        policy, ranked = self.plan(req)
        if not ranked:
            raise NoCandidatesError("no model can serve this request")

        res = Result(policy=policy.name)
        deadline = asyncio.get_running_loop().time() + timeout if timeout else None
        try:
            async with asyncio.timeout_at(deadline):
                return await self._walk(ranked, req, res, deadline)
        except TimeoutError as e:
            raise BudgetExhaustedError(f"request budget exhausted after {timeout:g}s", res) from e

    async def _walk(
        self, ranked: list[Candidate], req: ChatRequest, res: Result, deadline: float | None
    ) -> Result:
        last_err: Exception | None = None
        for i, c in enumerate(ranked):
            try:
                resp = await self._try_candidate(c, req, res, deadline)
            except Exception as err:
                last_err = err
                if i < len(ranked) - 1:
                    self.metrics.observe_fallback(c.provider.name, c.model.name)
                    # Skipping an open breaker happens on every request during an
                    # outage; the state change itself is already logged once.
                    level = logging.DEBUG if isinstance(err, BreakerOpenError) else logging.WARNING
                    self.log.log(
                        level,
                        "candidate failed, falling back",
                        extra={"failed": c.key, "next": ranked[i + 1].key, "error": describe(err)},
                    )
                continue

            res.provider, res.model, res.response = c.provider.name, c.model.name, resp
            res.cost_usd = round(c.model.cost(resp.input_tokens, resp.output_tokens), 10)
            self.metrics.observe_usage(
                c.provider.name, c.model.name, resp.input_tokens, resp.output_tokens, res.cost_usd
            )
            return res

        assert last_err is not None
        raise AllFailedError(f"all providers failed: last error: {describe(last_err)}", res)

    async def _try_candidate(
        self, c: Candidate, req: ChatRequest, res: Result, deadline: float | None
    ) -> Response:
        """Calls one candidate with retries, recording every attempt. Each
        attempt first asks the provider's circuit breaker for permission, so a
        breaker that opens mid-retry stops the retries too."""
        name, model = c.provider.name, c.model.name
        breaker = self.breakers[name]
        loop = asyncio.get_running_loop()

        def on_retry(attempt: int, err: Exception, delay: float) -> None:
            self.metrics.observe_retry(name, model)
            self.log.info(
                "retrying",
                extra={
                    "candidate": c.key,
                    "attempt": attempt,
                    "delay_ms": round(delay * 1000),
                    "error": describe(err),
                },
            )

        async def attempt() -> Response:
            a = Attempt(provider=name, model=model)
            res.attempts.append(a)
            if not breaker.allow():
                # Not retryable by classify(), so the router moves straight to the next candidate.
                err = BreakerOpenError(name)
                a.error = str(err)
                self.metrics.observe_attempt(name, model, m.OUTCOME_BREAKER_OPEN, 0)
                raise err

            start = time.perf_counter()
            try:
                try:
                    async with asyncio.timeout(c.timeout or None):
                        resp = await c.provider.chat(
                            Request(model=model, messages=req.messages, max_tokens=req.max_tokens)
                        )
                except TimeoutError as e:
                    raise AttemptTimeoutError(name, c.timeout) from e
            except asyncio.CancelledError:
                # The request was cancelled or ran out of time: that says
                # nothing about the provider, so just free the probe slot.
                breaker.release()
                timed_out = deadline is not None and loop.time() >= deadline
                a.duration_ms = _ms_since(start)
                a.error = "request budget exhausted" if timed_out else "request cancelled"
                outcome = m.OUTCOME_TIMEOUT if timed_out else m.OUTCOME_CANCELED
                self.metrics.observe_attempt(name, model, outcome, time.perf_counter() - start)
                raise
            except Exception as err:
                retryable, _ = classify(err)
                if retryable:
                    breaker.failure()  # 5xx, 429, timeout, network error
                else:
                    breaker.success()  # e.g. 400/401: the provider is up, it rejected this request
                a.duration_ms = _ms_since(start)
                a.error = describe(err)
                self.metrics.observe_attempt(name, model, _outcome(err), time.perf_counter() - start)
                raise

            elapsed = time.perf_counter() - start
            breaker.success()
            self.latency.observe(name, elapsed)
            a.duration_ms = _ms_since(start)
            self.metrics.observe_attempt(name, model, m.OUTCOME_SUCCESS, elapsed)
            return resp

        policy = dataclasses.replace(self.retry_policy, on_retry=on_retry)
        return await retry(attempt, policy, deadline=deadline)


def _ms_since(start: float) -> int:
    return int((time.perf_counter() - start) * 1000)


def _outcome(err: Exception) -> str:
    """Maps an attempt's error to the metrics "outcome" label."""
    if isinstance(err, ProviderError) and err.status_code == 429:
        return m.OUTCOME_RATE_LIMITED
    if isinstance(err, AttemptTimeoutError):
        return m.OUTCOME_TIMEOUT
    return m.OUTCOME_ERROR

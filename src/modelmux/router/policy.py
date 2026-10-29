"""Routing policies. A policy orders the candidates for a request: the first
is the primary choice and the rest form the fallback chain. Policies never call
providers, so they are cheap to run and easy to test."""

from __future__ import annotations

from collections.abc import Sequence
from dataclasses import dataclass
from typing import TYPE_CHECKING, Protocol

from ..config import Rule

if TYPE_CHECKING:
    from .router import Candidate


@dataclass(frozen=True)
class RouteInfo:
    """What a policy knows about a request."""

    prompt_tokens: int  # estimated
    max_tokens: int


class Policy(Protocol):
    name: str

    def rank(self, info: RouteInfo, cands: Sequence[Candidate]) -> list[Candidate]: ...


def estimated_cost(c: Candidate, info: RouteInfo) -> float:
    """Assumes the full max_tokens are generated, so it is an upper bound."""
    return c.model.cost(info.prompt_tokens, info.max_tokens)


# sorted() is stable, so candidates that tie keep their configured order.


class CostPolicy:
    """Prefers the cheapest model."""

    name = "cost"

    def rank(self, info: RouteInfo, cands: Sequence[Candidate]) -> list[Candidate]:
        return sorted(cands, key=lambda c: estimated_cost(c, info))


class QualityPolicy:
    """Prefers the highest-quality model, then the cheapest."""

    name = "quality"

    def rank(self, info: RouteInfo, cands: Sequence[Candidate]) -> list[Candidate]:
        return sorted(cands, key=lambda c: (-c.model.quality, estimated_cost(c, info)))


class LatencySource(Protocol):
    def p95(self, provider: str) -> float | None: ...


class LatencyPolicy:
    """Prefers the provider with the lowest recent p95 latency. Providers with
    no samples yet sort first (treated as 0), so every provider gets measured
    instead of being starved by an early winner."""

    name = "latency"

    def __init__(self, stats: LatencySource) -> None:
        self.stats = stats

    def rank(self, info: RouteInfo, cands: Sequence[Candidate]) -> list[Candidate]:
        return sorted(cands, key=lambda c: (self.stats.p95(c.provider.name) or 0.0, estimated_cost(c, info)))


class RulePolicy:
    """Applies the first matching rule from config: its preferred models go to
    the front, in the listed order, followed by everything else in base order."""

    name = "rule"

    def __init__(self, rules: Sequence[Rule], base: Policy) -> None:
        self.rules = list(rules)
        self.base = base

    def rank(self, info: RouteInfo, cands: Sequence[Candidate]) -> list[Candidate]:
        ranked = self.base.rank(info, cands)
        for rule in self.rules:
            if info.prompt_tokens >= rule.min_prompt_tokens:
                return prefer_first(ranked, rule.prefer)
        return ranked


def prefer_first(ranked: Sequence[Candidate], keys: Sequence[str]) -> list[Candidate]:
    by_key = {c.key: c for c in ranked}
    front = [by_key[k] for k in dict.fromkeys(keys) if k in by_key]  # dict.fromkeys drops duplicates
    chosen = {c.key for c in front}
    return front + [c for c in ranked if c.key not in chosen]


def fitting(info: RouteInfo, cands: Sequence[Candidate]) -> list[Candidate]:
    """Drops candidates whose context window cannot hold the prompt plus the
    requested output."""
    return [c for c in cands if info.prompt_tokens + info.max_tokens <= c.model.context_window]

import pytest

from helpers import cand, make_router
from modelmux.config import Rule
from modelmux.providers import Message
from modelmux.router import (
    ChatRequest,
    CostPolicy,
    LatencyPolicy,
    LatencyTracker,
    NoCandidatesError,
    QualityPolicy,
    RouteInfo,
    RulePolicy,
    fitting,
)

# Deliberately not in any sorted order.
CANDS = [
    cand("premium", "large", 200_000, 0.003, 0.015, 3),
    cand("cheap", "small", 8_000, 0.0002, 0.0008, 1),
    cand("mid", "medium", 32_000, 0.001, 0.003, 2),
]
SHORT = RouteInfo(prompt_tokens=100, max_tokens=256)
LONG = RouteInfo(prompt_tokens=5000, max_tokens=256)
RULES = [Rule(name="long", min_prompt_tokens=3000, prefer=["premium/large"])]


class FakeLatency(dict):
    def p95(self, provider: str) -> float | None:
        return self.get(provider)


def keys(cands) -> list[str]:
    return [c.key for c in cands]


@pytest.mark.parametrize(
    ("policy", "info", "want"),
    [
        (CostPolicy(), SHORT, ["cheap/small", "mid/medium", "premium/large"]),
        (QualityPolicy(), SHORT, ["premium/large", "mid/medium", "cheap/small"]),
        # unmeasured providers sort first, then lowest p95
        (
            LatencyPolicy(FakeLatency(premium=0.05, cheap=0.3)),
            SHORT,
            ["mid/medium", "premium/large", "cheap/small"],
        ),
        # ties broken by cost
        (
            LatencyPolicy(FakeLatency(premium=1, cheap=1, mid=1)),
            SHORT,
            ["cheap/small", "mid/medium", "premium/large"],
        ),
        (RulePolicy(RULES, CostPolicy()), SHORT, ["cheap/small", "mid/medium", "premium/large"]),
        (RulePolicy(RULES, CostPolicy()), LONG, ["premium/large", "cheap/small", "mid/medium"]),
    ],
    ids=["cost", "quality", "latency", "latency ties", "rule short prompt", "rule long prompt"],
)
def test_policies(policy, info: RouteInfo, want: list[str]) -> None:
    assert keys(policy.rank(info, CANDS)) == want


def test_policy_does_not_mutate_input() -> None:
    before = keys(CANDS)
    CostPolicy().rank(RouteInfo(0, 10), CANDS)
    assert keys(CANDS) == before


def test_fitting_drops_small_context_windows() -> None:
    assert keys(fitting(RouteInfo(prompt_tokens=10_000, max_tokens=512), CANDS)) == [
        "premium/large",
        "mid/medium",
    ]


async def test_hint_overrides_default_policy() -> None:
    r = make_router(*CANDS)
    msgs = [Message("user", "hi")]

    res = await r.chat(ChatRequest(msgs, 50))
    assert (res.model, res.policy) == ("small", "cost")

    res = await r.chat(ChatRequest(msgs, 50, hint="quality"))
    assert (res.model, res.policy) == ("large", "quality")


async def test_rejects_prompt_larger_than_every_model() -> None:
    huge = "x" * 1_000_000  # ~250K tokens
    with pytest.raises(NoCandidatesError):
        await make_router(*CANDS).chat(ChatRequest([Message("user", huge)], 50))


def test_latency_tracker_p95() -> None:
    lt = LatencyTracker(100)
    assert lt.p95("a") is None
    for i in range(1, 101):
        lt.observe("a", i / 1000)
    assert lt.p95("a") == 0.095


def test_latency_tracker_window_ages_out_old_samples() -> None:
    lt = LatencyTracker(10)
    for _ in range(10):
        lt.observe("a", 1.0)  # the provider was slow...
    for _ in range(10):
        lt.observe("a", 0.001)  # ...and has recovered
    assert lt.p95("a") == 0.001

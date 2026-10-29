"""Integration tests over the full HTTP stack: middleware, router, retries,
breakers and metrics, in front of two mock providers. "a" is cheaper, so the
cost policy always tries it first, and "b" is the fallback."""

from collections.abc import AsyncIterator

import httpx
import pytest

from modelmux.config import BreakerConfig, ModelConfig, RetryConfig, RoutingConfig, ServerConfig
from modelmux.metrics import Metrics
from modelmux.providers import Mock, MockConfig
from modelmux.router import AllFailedError, BudgetExhaustedError, Candidate, NoCandidatesError, Router
from modelmux.server import create_app, status_for

HI = {"messages": [{"role": "user", "content": "hi"}]}


def gateway(a: MockConfig, b: MockConfig) -> httpx.AsyncClient:
    mock_a, mock_b = Mock("a", a), Mock("b", b)
    cands = [
        Candidate(mock_a, ModelConfig(name="a-model", context_window=8000, input_price_per_1k=0.001)),
        Candidate(mock_b, ModelConfig(name="b-model", context_window=8000, input_price_per_1k=0.01)),
    ]
    metrics = Metrics()
    router = Router(
        cands,
        RoutingConfig(policy="cost", latency_window=10),
        RetryConfig(max_attempts=2, base_delay=0.001, max_delay=0.005, max_retry_after=1),
        BreakerConfig(failure_threshold=5, open_timeout=60),
        metrics=metrics,
    )
    app = create_app(
        router,
        ServerConfig(request_timeout=5, default_max_tokens=64),
        metrics,
        mocks={"a": mock_a, "b": mock_b},
    )
    return httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://gateway")


@pytest.fixture
async def healthy() -> AsyncIterator[httpx.AsyncClient]:
    async with gateway(MockConfig(), MockConfig()) as c:
        yield c


async def test_fallback_to_second_provider() -> None:
    # Provider A fails, the request falls back to B and succeeds, and the
    # fallback is visible in the response and in the metrics.
    async with gateway(MockConfig(error_rate=1), MockConfig()) as c:
        resp = await c.post("/v1/chat", json=HI)
        body = resp.json()
        assert resp.status_code == 200, body
        assert (body["provider"], body["model"]) == ("b", "b-model")
        assert len(body["attempts"]) == 3  # a, a (retry), b
        assert resp.headers["x-request-id"] == body["request_id"]

        metrics = (await c.get("/metrics")).text
    for want in [
        'modelmux_fallbacks_total{model="a-model",provider="a"} 1.0',
        'modelmux_upstream_requests_total{model="a-model",outcome="error",provider="a"} 2.0',
        'modelmux_upstream_requests_total{model="b-model",outcome="success",provider="b"} 1.0',
        'modelmux_http_requests_total{code="200"} 1.0',
    ]:
        assert want in metrics


async def test_all_providers_fail() -> None:
    async with gateway(MockConfig(error_rate=1), MockConfig(error_rate=1)) as c:
        resp = await c.post("/v1/chat", json=HI)
    assert resp.status_code == 502
    assert "all providers failed" in resp.json()["error"]
    assert len(resp.json()["attempts"]) == 4


async def test_admin_outage_and_recovery(healthy: httpx.AsyncClient) -> None:
    async def served_by() -> str:
        return (await healthy.post("/v1/chat", json=HI)).json()["provider"]

    assert await served_by() == "a"
    assert (await healthy.patch("/admin/mocks/a", json={"error_rate": 1})).status_code == 200
    assert await served_by() == "b"
    assert (await healthy.patch("/admin/mocks/a", json={"error_rate": 0})).status_code == 200
    assert await served_by() == "a"


@pytest.mark.parametrize(
    "body",
    [
        "{",
        '{"messages":[]}',
        '{"messages":[{"role":"robot","content":"hi"}]}',
        '{"messages":[{"role":"user","content":"hi"}],"routing_hint":"turbo"}',
        '{"messages":[{"role":"user","content":"hi"}],"temperature":2}',
        '{"messages":[{"role":"user","content":"hi"}],"max_tokens":-1}',
    ],
    ids=["not JSON", "no messages", "bad role", "bad hint", "unknown field", "negative max_tokens"],
)
async def test_chat_validation(healthy: httpx.AsyncClient, body: str) -> None:
    resp = await healthy.post("/v1/chat", content=body)
    assert resp.status_code == 400
    assert resp.json()["error"]


async def test_wrong_method(healthy: httpx.AsyncClient) -> None:
    assert (await healthy.get("/v1/chat")).status_code == 405


async def test_admin_rejects_bad_updates(healthy: httpx.AsyncClient) -> None:
    assert (await healthy.patch("/admin/mocks/nope", json={})).status_code == 404
    assert (await healthy.patch("/admin/mocks/a", json={"error_rate": 2})).status_code == 400
    assert (await healthy.patch("/admin/mocks/a", json={"latency": "soon"})).status_code == 400


async def test_request_id_from_client_is_kept_only_if_safe(healthy: httpx.AsyncClient) -> None:
    async def send(rid: str) -> str:
        return (await healthy.post("/v1/chat", json=HI, headers={"X-Request-ID": rid})).headers[
            "x-request-id"
        ]

    assert await send("trace-123") == "trace-123"
    unsafe = await send("bad id with spaces")
    assert unsafe and unsafe != "bad id with spaces"


@pytest.mark.parametrize(
    ("err", "status"),
    [
        (NoCandidatesError("no model"), 422),
        (BudgetExhaustedError("budget"), 504),
        (AllFailedError("all failed"), 502),
    ],
)
def test_status_for(err: Exception, status: int) -> None:
    assert status_for(err) == status

import json
from datetime import UTC, datetime, timedelta
from email.utils import format_datetime

import httpx
import pytest

from modelmux.providers import (
    Anthropic,
    AttemptTimeoutError,
    Message,
    OpenAI,
    ProviderError,
    Request,
    classify,
)
from modelmux.providers.anthropic import ANTHROPIC_VERSION
from modelmux.providers.httpjson import retry_after


def client(handler) -> httpx.AsyncClient:
    """An httpx client whose requests go to handler instead of the network."""
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


async def test_anthropic_chat() -> None:
    def handler(req: httpx.Request) -> httpx.Response:
        assert req.url.path == "/v1/messages"
        assert req.headers["x-api-key"] == "test-key"
        assert req.headers["anthropic-version"] == ANTHROPIC_VERSION
        body = json.loads(req.content)
        # The system prompt moves out of messages into the top-level field.
        assert body["system"] == "be brief"
        assert body["messages"] == [{"role": "user", "content": "hi"}]
        return httpx.Response(
            200,
            json={
                "content": [{"type": "thinking", "thinking": "..."}, {"type": "text", "text": "hello"}],
                "usage": {"input_tokens": 12, "output_tokens": 3},
            },
        )

    async with client(handler) as c:
        resp = await Anthropic("anthropic", "http://test", "test-key", c).chat(
            Request("claude-x", [Message("system", "be brief"), Message("user", "hi")], 10)
        )
    assert (resp.content, resp.input_tokens, resp.output_tokens) == ("hello", 12, 3)


async def test_openai_chat() -> None:
    def handler(req: httpx.Request) -> httpx.Response:
        assert req.url.path == "/v1/chat/completions"
        assert req.headers["authorization"] == "Bearer test-key"
        return httpx.Response(
            200,
            json={
                "choices": [{"message": {"content": "hello"}}],
                "usage": {"prompt_tokens": 12, "completion_tokens": 3},
            },
        )

    async with client(handler) as c:
        resp = await OpenAI("openai", "http://test", "test-key", c).chat(
            Request("gpt-x", [Message("user", "hi")], 10)
        )
    assert (resp.content, resp.input_tokens, resp.output_tokens) == ("hello", 12, 3)


@pytest.mark.parametrize(
    ("status", "headers", "retryable", "after"),
    [
        (429, {"Retry-After": "2"}, True, 2.0),
        (429, {"Retry-After-Ms": "250"}, True, 0.25),
        (503, {}, True, 0.0),
        (529, {}, True, 0.0),  # Anthropic "overloaded"
        (400, {}, False, 0.0),
        (401, {}, False, 0.0),
    ],
)
async def test_http_errors_are_classified(status: int, headers: dict, retryable: bool, after: float) -> None:
    async with client(lambda _: httpx.Response(status, headers=headers, json={"error": "nope"})) as c:
        with pytest.raises(ProviderError) as exc:
            await Anthropic("anthropic", "http://test", "k", c).chat(Request("m", [Message("user", "hi")], 1))
    assert exc.value.status_code == status
    assert classify(exc.value) == (retryable, after)


def test_retry_after_http_date() -> None:
    now = datetime(2026, 1, 1, 12, tzinfo=UTC)
    headers = httpx.Headers({"Retry-After": format_datetime(now + timedelta(seconds=30), usegmt=True)})
    assert retry_after(headers, now.timestamp()) == 30


def test_classify_other_errors() -> None:
    assert classify(AttemptTimeoutError("p", 1.0)) == (True, 0.0)
    assert classify(httpx.ConnectError("refused")) == (True, 0.0)
    assert classify(ValueError("bad response")) == (False, 0.0)

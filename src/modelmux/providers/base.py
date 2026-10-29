"""The interface every LLM backend implements, plus error classification."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Protocol

import httpx


@dataclass(frozen=True)
class Message:
    """One turn of a conversation. role is "system", "user" or "assistant"."""

    role: str
    content: str


@dataclass(frozen=True)
class Request:
    """The provider-neutral input to a chat call."""

    model: str
    messages: list[Message]
    max_tokens: int


@dataclass(frozen=True)
class Response:
    """The provider-neutral output of a chat call."""

    content: str
    input_tokens: int
    output_tokens: int


class Provider(Protocol):
    """Implemented by every backend. Implementations must stop work when the
    calling task is cancelled, and should raise ProviderError for upstream
    failures so the caller can decide whether a retry makes sense."""

    @property
    def name(self) -> str: ...

    async def chat(self, req: Request) -> Response: ...


class ProviderError(Exception):
    """A failed upstream call that got an HTTP response."""

    def __init__(
        self, provider: str, status_code: int, message: str, *, retryable: bool, retry_after: float = 0.0
    ) -> None:
        super().__init__(f"{provider}: status {status_code}: {message}")
        self.provider = provider
        self.status_code = status_code
        self.retryable = retryable
        self.retry_after = retry_after  # seconds, from a Retry-After header; 0 if absent


class AttemptTimeoutError(Exception):
    """A single upstream call ran past the provider's attempt_timeout."""

    def __init__(self, provider: str, timeout: float) -> None:
        super().__init__(f"{provider}: attempt timed out after {timeout:g}s")


def is_retryable_status(code: int) -> bool:
    """Timeouts, rate limits and server-side failures are worth retrying; client
    errors like 400 or 401 are not."""
    return code in (408, 429, 500, 502, 503, 504, 529)  # 529 is Anthropic's "overloaded"


def classify(err: BaseException) -> tuple[bool, float]:
    """Decides whether an error from a provider call is transient, and returns
    any delay the provider asked for. It is the bridge between provider errors
    and the generic retry loop in modelmux.resilience."""
    if isinstance(err, ProviderError):
        return err.retryable, err.retry_after
    if isinstance(err, AttemptTimeoutError):
        return True, 0.0  # one attempt timed out; the retry loop checks the overall budget
    if isinstance(err, httpx.TransportError):
        return True, 0.0  # connection reset, DNS failure, read timeout, etc.
    return False, 0.0


def describe(err: BaseException) -> str:
    """A readable one-line error, even for exceptions with an empty message."""
    return str(err) or type(err).__name__


def estimate_tokens(messages: list[Message]) -> int:
    """A rough token count: about 4 characters per token plus a small
    per-message overhead. It is only used for routing decisions and cost
    estimates, so being approximate is fine and avoids a tokenizer dependency."""
    return sum(4 + (len(m.content) + 3) // 4 for m in messages)

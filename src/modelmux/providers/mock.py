from __future__ import annotations

import asyncio
import json
import random
from dataclasses import dataclass

from .base import Message, ProviderError, Request, Response, estimate_tokens


@dataclass(frozen=True)
class MockConfig:
    """How a Mock behaves. error_rate and rate_limit_rate are probabilities in [0, 1]."""

    latency: float = 0.0  # seconds
    jitter: float = 0.0  # extra random latency in [0, jitter)
    error_rate: float = 0.0  # chance of a 503
    rate_limit_rate: float = 0.0  # chance of a 429
    retry_after: float = 0.0  # Retry-After sent with 429s


class Mock:
    """A fake provider with configurable latency and failures, so the whole
    gateway can run and be tested without API keys."""

    def __init__(self, name: str, config: MockConfig | None = None) -> None:
        self._name = name
        # Replaced as a whole (the dataclass is frozen), so a request in flight
        # always sees one consistent config even if the admin endpoint changes it.
        self.config = config or MockConfig()

    @property
    def name(self) -> str:
        return self._name

    async def chat(self, req: Request) -> Response:
        cfg = self.config
        # Simulate the upstream call. asyncio.sleep is cancelled with the task,
        # so a timeout or client disconnect cuts it short.
        await asyncio.sleep(cfg.latency + random.uniform(0, cfg.jitter))

        roll = random.random()
        if roll < cfg.rate_limit_rate:
            raise ProviderError(
                self._name, 429, "mock rate limit", retryable=True, retry_after=cfg.retry_after
            )
        if roll < cfg.rate_limit_rate + cfg.error_rate:
            raise ProviderError(self._name, 503, "mock upstream failure", retryable=True)

        prompt = json.dumps(_truncate(_last_user_message(req.messages), 60), ensure_ascii=False)
        content = f"[{self._name}/{req.model}] mock reply to: {prompt}"
        out = min(estimate_tokens([Message("assistant", content)]), req.max_tokens)
        return Response(content=content, input_tokens=estimate_tokens(req.messages), output_tokens=out)


def _last_user_message(messages: list[Message]) -> str:
    return next((m.content for m in reversed(messages) if m.role == "user"), "")


def _truncate(s: str, n: int) -> str:
    return s if len(s) <= n else s[:n] + "..."

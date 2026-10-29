from __future__ import annotations

import httpx

from .base import Request, Response
from .httpjson import post_json

ANTHROPIC_VERSION = "2023-06-01"


class Anthropic:
    """Calls the Claude Messages API (POST /v1/messages)."""

    def __init__(self, name: str, base_url: str, api_key: str, client: httpx.AsyncClient) -> None:
        # client should be shared and long-lived so connections are reused.
        self._name = name
        self._base_url = (base_url or "https://api.anthropic.com").rstrip("/")
        self._api_key = api_key
        self._client = client

    @property
    def name(self) -> str:
        return self._name

    async def chat(self, req: Request) -> Response:
        # Anthropic takes the system prompt as a top-level field, not as a message.
        system = [m.content for m in req.messages if m.role == "system"]
        body: dict = {
            "model": req.model,
            "max_tokens": req.max_tokens,
            "messages": [{"role": m.role, "content": m.content} for m in req.messages if m.role != "system"],
        }
        if system:
            body["system"] = "\n\n".join(system)

        headers = {"x-api-key": self._api_key, "anthropic-version": ANTHROPIC_VERSION}
        out = await post_json(self._client, self._name, f"{self._base_url}/v1/messages", headers, body)

        # Keep only text blocks; thinking blocks and others are not part of the reply.
        text = "".join(b.get("text", "") for b in out.get("content", []) if b.get("type") == "text")
        usage = out.get("usage", {})
        return Response(
            content=text,
            input_tokens=usage.get("input_tokens", 0),
            output_tokens=usage.get("output_tokens", 0),
        )

from __future__ import annotations

import httpx

from .base import Request, Response
from .httpjson import post_json


class OpenAI:
    """Calls the Chat Completions API (POST /v1/chat/completions)."""

    def __init__(self, name: str, base_url: str, api_key: str, client: httpx.AsyncClient) -> None:
        # client should be shared and long-lived so connections are reused.
        self._name = name
        self._base_url = (base_url or "https://api.openai.com").rstrip("/")
        self._api_key = api_key
        self._client = client

    @property
    def name(self) -> str:
        return self._name

    async def chat(self, req: Request) -> Response:
        body = {
            "model": req.model,
            # OpenAI accepts system messages inline.
            "messages": [{"role": m.role, "content": m.content} for m in req.messages],
            "max_completion_tokens": req.max_tokens,
        }
        headers = {"Authorization": f"Bearer {self._api_key}"}
        out = await post_json(
            self._client, self._name, f"{self._base_url}/v1/chat/completions", headers, body
        )

        choices = out.get("choices") or []
        if not choices:
            raise ValueError(f"{self._name}: response has no choices")
        usage = out.get("usage", {})
        return Response(
            content=choices[0].get("message", {}).get("content") or "",
            input_tokens=usage.get("prompt_tokens", 0),
            output_tokens=usage.get("completion_tokens", 0),
        )

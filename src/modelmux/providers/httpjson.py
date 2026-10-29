from __future__ import annotations

import email.utils
import time
from typing import Any

import httpx

from .base import ProviderError, is_retryable_status


async def post_json(
    client: httpx.AsyncClient, provider: str, url: str, headers: dict[str, str], body: dict[str, Any]
) -> Any:
    """Sends body as JSON and returns the decoded 2xx response. A non-2xx
    response becomes ProviderError, with retryable and retry_after filled in
    from the status code and headers. Network errors are raised as-is
    (httpx.TransportError); classify() knows how to treat them."""
    resp = await client.post(url, json=body, headers=headers)
    if not resp.is_success:
        raise ProviderError(
            provider,
            resp.status_code,
            resp.text[:512].strip(),
            retryable=is_retryable_status(resp.status_code),
            retry_after=retry_after(resp.headers, time.time()),
        )
    return resp.json()


def retry_after(headers: httpx.Headers, now: float) -> float:
    """How long the server asked us to wait, in seconds. Understands OpenAI's
    retry-after-ms and the standard Retry-After header, which is either a
    number of seconds or an HTTP date. Returns 0 if there is no usable value."""
    ms = headers.get("retry-after-ms", "")
    if ms.isdigit() and int(ms) > 0:
        return int(ms) / 1000
    value = headers.get("retry-after", "")
    if not value:
        return 0.0
    if value.isdigit():
        return float(value)
    try:
        when = email.utils.parsedate_to_datetime(value).timestamp()
    except (TypeError, ValueError):
        return 0.0
    return max(0.0, when - now)

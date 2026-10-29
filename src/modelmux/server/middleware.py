from __future__ import annotations

import re

from starlette.types import ASGIApp, Message, Receive, Scope, Send

from ..logs import new_request_id, request_id_var

# Short IDs of safe characters only, so a client cannot inject newlines or huge
# values into our logs.
_SAFE_ID = re.compile(r"[A-Za-z0-9._-]{1,64}")


def valid_request_id(rid: str) -> bool:
    return bool(_SAFE_ID.fullmatch(rid))


class RequestIDMiddleware:
    """Gives every request an ID: the caller's X-Request-ID if it is
    well-formed, otherwise a new random one. The ID goes into a ContextVar (so
    every log line carries it) and back to the caller as a response header.

    Written as plain ASGI rather than BaseHTTPMiddleware so it adds no extra
    task or body buffering in front of the handler.
    """

    def __init__(self, app: ASGIApp) -> None:
        self.app = app

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return

        sent = dict(scope["headers"]).get(b"x-request-id", b"").decode("latin-1")
        rid = sent if valid_request_id(sent) else new_request_id()

        async def send_with_id(message: Message) -> None:
            if message["type"] == "http.response.start":
                message.setdefault("headers", [])
                message["headers"].append((b"x-request-id", rid.encode()))
            await send(message)

        token = request_id_var.set(rid)
        try:
            await self.app(scope, receive, send_with_id)
        finally:
            request_id_var.reset(token)

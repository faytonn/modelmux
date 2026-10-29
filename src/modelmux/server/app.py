"""Exposes the router over HTTP."""

from __future__ import annotations

import asyncio
import contextlib
import logging
import time
from typing import Any, Literal

from fastapi import FastAPI, Request, Response
from fastapi.responses import JSONResponse
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest
from pydantic import BaseModel, ConfigDict, Field, ValidationError

from ..config import ServerConfig
from ..logs import request_id_var
from ..metrics import Metrics
from ..providers import Message, Mock
from ..router import (
    AllFailedError,
    BudgetExhaustedError,
    ChatRequest,
    NoCandidatesError,
    Router,
    RouterError,
)
from .admin import admin_routes
from .middleware import RequestIDMiddleware

MAX_BODY_BYTES = 1 << 20  # 1 MiB

# nginx's non-standard 499: the client hung up before we answered. Using it
# keeps cancelled requests from showing up as 502s (provider failures) in metrics.
STATUS_CLIENT_CLOSED_REQUEST = 499


class ChatMessage(BaseModel):
    model_config = ConfigDict(extra="forbid")

    role: Literal["system", "user", "assistant"]
    content: str


class ChatBody(BaseModel):
    # Unknown fields are rejected, so a typo like "max_token" is an error
    # instead of being silently ignored.
    model_config = ConfigDict(extra="forbid")

    messages: list[ChatMessage] = Field(min_length=1)
    max_tokens: int = Field(default=0, ge=0)  # 0 means server.default_max_tokens
    routing_hint: Literal["cheap", "fast", "quality"] | None = None


def status_for(err: RouterError) -> int:
    """Maps router errors to HTTP status codes."""
    if isinstance(err, NoCandidatesError):
        return 422
    if isinstance(err, BudgetExhaustedError):
        return 504
    if isinstance(err, AllFailedError):
        return 502
    return 500


def create_app(
    router: Router,
    config: ServerConfig,
    metrics: Metrics,
    *,
    mocks: dict[str, Mock] | None = None,
    log: logging.Logger | None = None,
) -> FastAPI:
    """Builds the ASGI app. mocks enables PATCH /admin/mocks/{name} for those
    providers; leave it None in production, the endpoint has no authentication."""
    log = log or logging.getLogger("modelmux")
    # No /docs: the API is three endpoints and documented in the README.
    app = FastAPI(title="ModelMux", docs_url=None, redoc_url=None, openapi_url=None)
    app.add_middleware(RequestIDMiddleware)

    @app.post("/v1/chat")
    async def chat(request: Request) -> JSONResponse:
        start = time.perf_counter()
        status, payload = await _serve_chat(request, router, config, log)
        elapsed = time.perf_counter() - start
        metrics.observe_http(status, elapsed)
        log.info(
            "http request",
            extra={
                "method": "POST",
                "path": "/v1/chat",
                "status": status,
                "duration_ms": round(elapsed * 1000),
            },
        )
        return JSONResponse(payload, status_code=status)

    @app.get("/healthz")
    async def healthz() -> Response:
        return Response(status_code=200)

    @app.get("/metrics")
    async def metrics_endpoint() -> Response:
        return Response(generate_latest(metrics.registry), media_type=CONTENT_TYPE_LATEST)

    if mocks is not None:
        app.include_router(admin_routes(mocks, log))
    return app


async def _serve_chat(
    request: Request, router: Router, config: ServerConfig, log: logging.Logger
) -> tuple[int, dict[str, Any]]:
    start = time.perf_counter()
    rid = request_id_var.get()

    if int(request.headers.get("content-length") or 0) > MAX_BODY_BYTES:
        return 413, {"request_id": rid, "error": "request body larger than 1 MiB"}
    try:
        body = ChatBody.model_validate_json(await request.body())
    except ValidationError as e:
        return 400, {"request_id": rid, "error": _first_error(e)}

    req = ChatRequest(
        messages=[Message(m.role, m.content) for m in body.messages],
        max_tokens=body.max_tokens or config.default_max_tokens,
        hint=body.routing_hint or "",
    )

    # The router runs in its own task so that a client disconnect can cancel
    # it, and with it every in-flight upstream call. request_timeout bounds the
    # whole request, including any retries and fallbacks.
    chat_task = asyncio.create_task(router.chat(req, timeout=config.request_timeout))
    gone_task = asyncio.create_task(_wait_for_disconnect(request))
    try:
        done, _ = await asyncio.wait({chat_task, gone_task}, return_when=asyncio.FIRST_COMPLETED)
    except asyncio.CancelledError:  # the server is shutting down
        chat_task.cancel()
        raise
    finally:
        gone_task.cancel()

    if chat_task not in done:  # the client went away first
        chat_task.cancel()
        with contextlib.suppress(asyncio.CancelledError, RouterError):
            await chat_task
        log.info("client disconnected, request cancelled")
        return STATUS_CLIENT_CLOSED_REQUEST, {"request_id": rid, "error": "client closed request"}

    try:
        res = chat_task.result()
    except RouterError as err:
        attempts = [a.to_dict() for a in err.result.attempts] if err.result else []
        log.warning("chat request failed", extra={"error": str(err), "attempts": len(attempts)})
        payload: dict[str, Any] = {"request_id": rid, "error": str(err)}
        if attempts:
            payload["attempts"] = attempts
        return status_for(err), payload

    assert res.response is not None
    log.info(
        "chat request served",
        extra={
            "provider": res.provider,
            "model": res.model,
            "policy": res.policy,
            "attempts": len(res.attempts),
            "cost_usd": res.cost_usd,
        },
    )
    return 200, {
        "request_id": rid,
        "provider": res.provider,
        "model": res.model,
        "routing_policy": res.policy,
        "content": res.response.content,
        "usage": {"input_tokens": res.response.input_tokens, "output_tokens": res.response.output_tokens},
        "estimated_cost_usd": res.cost_usd,
        "latency_ms": round((time.perf_counter() - start) * 1000),
        "attempts": [a.to_dict() for a in res.attempts],
    }


async def _wait_for_disconnect(request: Request) -> None:
    """Returns when the client disconnects. The body has already been read, so
    the next ASGI message the server sends is http.disconnect."""
    while (await request.receive())["type"] != "http.disconnect":
        pass


def _first_error(e: ValidationError) -> str:
    err = e.errors()[0]
    if err["type"] == "json_invalid":
        return f"invalid JSON body: {err['msg']}"
    where = ".".join(str(p) for p in err["loc"])
    return f"{where}: {err['msg']}" if where else err["msg"]

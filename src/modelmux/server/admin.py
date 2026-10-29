from __future__ import annotations

import dataclasses
import logging

from fastapi import APIRouter, Request
from fastapi.responses import JSONResponse
from pydantic import BaseModel, ConfigDict, ValidationError

from ..config import parse_duration
from ..providers import Mock, MockConfig


class MockUpdate(BaseModel):
    """A partial update: fields left out stay unchanged, which lets a client
    send {"error_rate": 1} without resetting everything else."""

    model_config = ConfigDict(extra="forbid")

    error_rate: float | None = None
    rate_limit_rate: float | None = None
    latency: str | None = None  # duration, e.g. "250ms"


def admin_routes(mocks: dict[str, Mock], log: logging.Logger) -> APIRouter:
    """PATCH /admin/mocks/{name} changes a mock provider's behaviour at
    runtime, so a demo or load test can start and end an outage. Only mounted
    when admin is enabled: it has no authentication."""
    routes = APIRouter()

    @routes.patch("/admin/mocks/{name}")
    async def update_mock(name: str, request: Request) -> JSONResponse:
        mock = mocks.get(name)
        if mock is None:
            return JSONResponse({"error": f"no mock provider named {name}"}, status_code=404)
        try:
            update = MockUpdate.model_validate_json(await request.body())
        except ValidationError as e:
            return JSONResponse({"error": f"invalid JSON body: {e.errors()[0]['msg']}"}, status_code=400)

        changes: dict[str, float] = update.model_dump(exclude_none=True, exclude={"latency"})
        if update.latency is not None:
            try:
                changes["latency"] = parse_duration(update.latency)
            except ValueError:
                return JSONResponse({"error": "latency must be a duration like 250ms"}, status_code=400)
        cfg = dataclasses.replace(mock.config, **changes)
        if not (0 <= cfg.error_rate <= 1 and 0 <= cfg.rate_limit_rate <= 1) or (
            cfg.error_rate + cfg.rate_limit_rate > 1
        ):
            return JSONResponse({"error": "rates must be in [0, 1] and sum to at most 1"}, status_code=400)

        mock.config = cfg
        log.warning(
            "mock provider reconfigured",
            extra={
                "provider": name,
                "error_rate": cfg.error_rate,
                "rate_limit_rate": cfg.rate_limit_rate,
                "latency_ms": round(cfg.latency * 1000),
            },
        )
        return JSONResponse(_state(cfg))

    return routes


def _state(cfg: MockConfig) -> dict[str, object]:
    return {
        "error_rate": cfg.error_rate,
        "rate_limit_rate": cfg.rate_limit_rate,
        "latency": f"{round(cfg.latency * 1000, 3):g}ms" if cfg.latency < 1 else f"{cfg.latency:g}s",
    }

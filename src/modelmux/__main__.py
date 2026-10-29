"""Runs the gateway: python -m modelmux --config config.yaml"""

from __future__ import annotations

import argparse
import asyncio
import logging
import os
import sys

import httpx
import uvicorn

from . import logs
from .config import Config, ConfigError, load
from .metrics import Metrics
from .providers import Anthropic, Mock, MockConfig, OpenAI, Provider
from .router import Candidate, Router
from .server import create_app


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="modelmux", description="Run the ModelMux LLM gateway.")
    parser.add_argument("--config", default="config.yaml", help="path to the YAML config file")
    parser.add_argument("--log-format", choices=["json", "text"], default="json", help="log format")
    parser.add_argument(
        "--admin",
        action="store_true",
        help="enable PATCH /admin/mocks/{name} (unauthenticated; for demos and load tests only)",
    )
    args = parser.parse_args(argv)

    log = logs.setup(args.log_format)
    try:
        asyncio.run(run(args.config, args.admin, log))
    except (ConfigError, ValueError) as e:
        log.error("modelmux exited", extra={"error": str(e)})
        return 1
    except KeyboardInterrupt:  # uvicorn re-raises Ctrl+C after a graceful shutdown
        pass
    return 0


async def run(config_path: str, admin: bool, log: logging.Logger) -> None:
    cfg = load(config_path)
    # One shared client: it pools connections. No client timeout; every call is
    # bounded by the router's asyncio timeouts instead.
    async with httpx.AsyncClient(timeout=None) as client:
        candidates, mocks = build_candidates(cfg, client, log)
        # A dedicated registry (instead of the global default) keeps metrics explicit.
        metrics = Metrics()
        router = Router(candidates, cfg.routing, cfg.retry, cfg.breaker, metrics=metrics, log=log)
        if admin:
            log.warning(
                "admin endpoint enabled: PATCH /admin/mocks/{name} is unauthenticated, do not expose it"
            )
        app = create_app(router, cfg.server, metrics, mocks=mocks if admin else None, log=log)

        host, port = parse_addr(cfg.server.addr)
        server = uvicorn.Server(
            uvicorn.Config(
                app,
                host=host,
                port=port,
                log_config=None,  # logs.setup already sends uvicorn's logs through our JSON handler
                access_log=False,  # the chat endpoint writes its own access log line with the request ID
                lifespan="off",
                # On Ctrl+C or SIGTERM (what `docker stop` sends), stop accepting
                # connections and give in-flight requests this long to finish.
                timeout_graceful_shutdown=10,
            )
        )
        log.info("modelmux starting", extra={"addr": cfg.server.addr, "candidates": len(candidates)})
        await server.serve()
        log.info("shut down")


def build_candidates(
    cfg: Config, client: httpx.AsyncClient, log: logging.Logger
) -> tuple[list[Candidate], dict[str, Mock]]:
    """Turns provider config into router candidates. A real provider whose API
    key is not set is skipped with a warning, so the same config works on a
    laptop without keys."""
    candidates: list[Candidate] = []
    mocks: dict[str, Mock] = {}
    for pc in cfg.providers:
        provider: Provider
        if pc.type == "mock":
            provider = Mock(pc.name, MockConfig(**pc.mock.model_dump()))
            mocks[pc.name] = provider
        else:
            key = os.environ.get(pc.api_key_env, "")
            if not key:
                log.warning(
                    "skipping provider: API key not set", extra={"provider": pc.name, "env": pc.api_key_env}
                )
                continue
            adapter = Anthropic if pc.type == "anthropic" else OpenAI
            provider = adapter(pc.name, pc.base_url, key, client)
        candidates += [Candidate(provider, m, pc.attempt_timeout) for m in pc.models]
    if not candidates:
        raise ConfigError("no usable providers: set an API key or add a mock provider")
    return candidates, mocks


def parse_addr(addr: str) -> tuple[str, int]:
    """ ":8080" listens on every interface; "127.0.0.1:8080" on one."""
    host, _, port = addr.rpartition(":")
    return host or "0.0.0.0", int(port)


if __name__ == "__main__":
    sys.exit(main())

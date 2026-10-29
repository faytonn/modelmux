"""Drives traffic at a running gateway in three phases (healthy, outage,
recovery) and prints once per second who served the requests, the error rate,
p95 latency and the target's circuit breaker state.

The outage is injected through the gateway's admin endpoint, so start the
gateway with --admin:

    python -m modelmux --admin
    python -m modelmux.loadtest
"""

from __future__ import annotations

import argparse
import asyncio
import contextlib
import math
import sys
import time
from collections import Counter
from dataclasses import dataclass, field

import httpx

from .config import parse_duration

BREAKER_NAMES = {"0": "closed", "1": "open", "2": "half-open"}
BODY = {"messages": [{"role": "user", "content": "load test"}], "max_tokens": 32}


@dataclass
class Stats:
    """One second's worth of results."""

    ok: int = 0
    failed: int = 0
    fallbacks: int = 0  # successful responses that needed more than one provider
    served_by: Counter[str] = field(default_factory=Counter)
    latencies: list[float] = field(default_factory=list)

    def record(self, provider: str | None, fell_back: bool, latency: float) -> None:
        self.latencies.append(latency)
        if provider is None:
            self.failed += 1
            return
        self.ok += 1
        self.served_by[provider] += 1
        if fell_back:
            self.fallbacks += 1


def p95(values: list[float]) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    return ordered[math.ceil(0.95 * len(ordered)) - 1]


def fmt_duration(seconds: float) -> str:
    return f"{seconds * 1000:.0f}ms" if seconds < 1 else f"{seconds:.2f}s"


async def send(client: httpx.AsyncClient) -> tuple[str | None, bool, float]:
    """Sends one chat request. Returns the provider that served it (None on
    failure), whether it needed a fallback, and the latency in seconds."""
    t0 = time.perf_counter()
    try:
        resp = await client.post("/v1/chat", json=BODY)
    except httpx.HTTPError:
        return None, False, time.perf_counter() - t0
    latency = time.perf_counter() - t0
    if resp.status_code != 200:
        return None, False, latency
    out = resp.json()
    attempts = out.get("attempts") or []
    return out["provider"], bool(attempts) and attempts[0]["provider"] != out["provider"], latency


async def scrape(client: httpx.AsyncClient, *prefixes: str) -> list[str]:
    """The /metrics lines starting with any of the given prefixes."""
    try:
        resp = await client.get("/metrics")
    except httpx.HTTPError:
        return []
    return [line for line in resp.text.splitlines() if line.startswith(prefixes)]


async def breaker_state(client: httpx.AsyncClient, provider: str) -> str:
    prefix = f'modelmux_circuit_breaker_state{{provider="{provider}"}} '
    for line in await scrape(client, prefix):
        value = line.removeprefix(prefix).split(".")[0]  # prometheus_client writes 1.0
        return BREAKER_NAMES.get(value, "?")
    return "?"


async def set_error_rate(client: httpx.AsyncClient, mock: str, rate: float) -> None:
    resp = await client.patch(f"/admin/mocks/{mock}", json={"error_rate": rate})
    if resp.status_code != 200:
        raise RuntimeError(f"PATCH /admin/mocks/{mock}: {resp.status_code} {resp.text}")


async def run_phase(
    client: httpx.AsyncClient, target: str, name: str, concurrency: int, duration: float, start: float
) -> None:
    """Runs concurrency workers until the phase ends, printing a summary line every second."""
    loop = asyncio.get_running_loop()
    end = loop.time() + duration
    stats = Stats()

    async def worker() -> None:
        # Requests still in flight when the phase ends are cancelled and not counted.
        with contextlib.suppress(TimeoutError):
            async with asyncio.timeout_at(end):
                while True:
                    result = await send(client)
                    # Read `stats` only now, after the await: a request that
                    # started in the previous second still counts in this one.
                    stats.record(*result)

    async def reporter() -> None:
        nonlocal stats
        tick = loop.time()
        while True:
            tick += 1
            if tick > end:
                return
            await asyncio.sleep(tick - loop.time())
            s, stats = stats, Stats()  # each line reports one second
            state = await breaker_state(client, target)
            total = s.ok + s.failed
            err_pct = 100 * s.failed / total if total else 0.0
            served = " ".join(f"{k}={v}" for k, v in sorted(s.served_by.items()))
            print(
                f"[{name:<8}] t={time.perf_counter() - start:3.0f}s rps={total:4d} err={err_pct:5.1f}% "
                f"p95={fmt_duration(p95(s.latencies)):>6} fallbacks={s.fallbacks:4d} "
                f"breaker({target})={state:<9} served: {served}",
                flush=True,
            )

    async with asyncio.TaskGroup() as tg:
        for _ in range(concurrency):
            tg.create_task(worker())
        tg.create_task(reporter())


async def run(url: str, concurrency: int, phase: float, target: str) -> int:
    limits = httpx.Limits(max_connections=concurrency + 4, max_keepalive_connections=concurrency + 4)
    async with httpx.AsyncClient(base_url=url, timeout=30, limits=limits) as client:
        try:
            await set_error_rate(client, target, 0)
        except (httpx.HTTPError, RuntimeError) as e:
            print(f"cannot reach the admin endpoint (start the gateway with --admin): {e}", file=sys.stderr)
            return 1

        print(
            f"load test: {concurrency} clients, {phase:g}s per phase, breaking {target!r} during the outage\n"
        )
        start = time.perf_counter()
        for name, error_rate in (("healthy", 0.0), ("outage", 1.0), ("recovery", 0.0)):
            await set_error_rate(client, target, error_rate)
            print(f"--- phase {name}: {target} error_rate={error_rate * 100:.0f}%", flush=True)
            await run_phase(client, target, name, concurrency, phase, start)

        print("\n--- gateway counters (from /metrics)")
        for line in await scrape(
            client,
            "modelmux_fallbacks_total",
            "modelmux_upstream_requests_total",
            "modelmux_circuit_breaker_state",
        ):
            print(line)
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="modelmux.loadtest", description=__doc__.split("\n\n")[0])
    parser.add_argument("--url", default="http://localhost:8080", help="gateway base URL")
    parser.add_argument("--concurrency", type=int, default=16, help="concurrent clients")
    parser.add_argument("--phase", type=parse_duration, default=15.0, help="duration of each phase, e.g. 12s")
    parser.add_argument(
        "--target", default="mock-cheap", help="mock provider to break during the outage phase"
    )
    args = parser.parse_args(argv)
    return asyncio.run(run(args.url, args.concurrency, args.phase, args.target))


if __name__ == "__main__":
    sys.exit(main())

"""Loads and validates the gateway's YAML configuration.

API keys are deliberately not part of the file: a provider names the
environment variable that holds its key instead.
"""

from __future__ import annotations

import re
from pathlib import Path
from typing import Annotated, Literal

import yaml
from pydantic import BaseModel, BeforeValidator, ConfigDict, Field, ValidationError

_DURATION_PART = re.compile(r"(\d+(?:\.\d+)?)(ms|s|m|h)")
_UNIT_SECONDS = {"ms": 0.001, "s": 1.0, "m": 60.0, "h": 3600.0}


def parse_duration(value: object) -> float:
    """Parses "250ms", "15s", "1m30s" (or a plain number of seconds) into seconds."""
    if isinstance(value, int | float) and not isinstance(value, bool):
        return float(value)
    if not isinstance(value, str):
        raise ValueError(f"duration must be a string like 250ms or 15s, got {value!r}")
    text = value.strip()
    if text in ("0", ""):
        return 0.0
    parts = _DURATION_PART.findall(text)
    if not parts or "".join(n + u for n, u in parts) != text:
        raise ValueError(f"invalid duration {value!r}: use units ms, s, m or h, e.g. 250ms or 1m30s")
    return sum(float(n) * _UNIT_SECONDS[u] for n, u in parts)


# A duration in config, stored as float seconds.
Duration = Annotated[float, BeforeValidator(parse_duration)]


class _Strict(BaseModel):
    # A typo in a key is an error, not a silently ignored field.
    model_config = ConfigDict(extra="forbid")


class ServerConfig(_Strict):
    addr: str = ":8080"
    request_timeout: Duration = 30.0  # budget for the whole request, fallbacks included
    default_max_tokens: int = 512


class Rule(_Strict):
    """Moves the `prefer` models ("provider/model") to the front of the
    fallback chain when the prompt has at least `min_prompt_tokens` tokens."""

    name: str
    min_prompt_tokens: int = 0
    prefer: list[str] = Field(default_factory=list)


class RoutingConfig(_Strict):
    policy: str = "cost"  # default policy: cost | latency | quality | rule
    latency_window: int = 100  # samples per provider used for p95
    rules: list[Rule] = Field(default_factory=list)  # used by the rule policy, first match wins


class RetryConfig(_Strict):
    """Applies to each candidate in the fallback chain separately."""

    max_attempts: int = 3  # per candidate, first try included
    base_delay: Duration = 0.1
    max_delay: Duration = 2.0
    max_retry_after: Duration = 5.0  # a longer Retry-After means fall back instead of waiting


class BreakerConfig(_Strict):
    failure_threshold: int = 5  # consecutive failures before opening
    open_timeout: Duration = 10.0  # how long to stay open before a probe


class MockSettings(_Strict):
    latency: Duration = 0.0
    jitter: Duration = 0.0
    error_rate: float = 0.0
    rate_limit_rate: float = 0.0
    retry_after: Duration = 0.0


class ModelConfig(_Strict):
    name: str
    context_window: int  # tokens
    input_price_per_1k: float = 0.0
    output_price_per_1k: float = 0.0
    quality: int = 0  # relative rank, higher is better

    def cost(self, input_tokens: int, output_tokens: int) -> float:
        """Estimated USD cost of a call with the given token counts."""
        return input_tokens / 1000 * self.input_price_per_1k + output_tokens / 1000 * self.output_price_per_1k


class ProviderConfig(_Strict):
    name: str
    type: Literal["mock", "anthropic", "openai"]
    base_url: str = ""
    api_key_env: str = ""
    attempt_timeout: Duration = 10.0  # budget for a single upstream call
    mock: MockSettings = Field(default_factory=MockSettings)
    models: list[ModelConfig] = Field(default_factory=list)


class Config(_Strict):
    server: ServerConfig = Field(default_factory=ServerConfig)
    routing: RoutingConfig = Field(default_factory=RoutingConfig)
    retry: RetryConfig = Field(default_factory=RetryConfig)
    breaker: BreakerConfig = Field(default_factory=BreakerConfig)
    providers: list[ProviderConfig] = Field(default_factory=list)


class ConfigError(Exception):
    pass


def load(path: str | Path) -> Config:
    """Reads, defaults and validates the config at path."""
    try:
        raw = yaml.safe_load(Path(path).read_text(encoding="utf-8")) or {}
    except (OSError, yaml.YAMLError) as e:
        raise ConfigError(f"read config {path}: {e}") from e
    try:
        cfg = Config.model_validate(raw)
    except ValidationError as e:
        raise ConfigError(f"parse config {path}: {_describe(e)}") from e
    problems = _validate(cfg)
    if problems:
        # Report every problem at once instead of one per restart.
        raise ConfigError(f"invalid config {path}:\n  " + "\n  ".join(problems))
    return cfg


def _describe(e: ValidationError) -> str:
    return "; ".join(f"{'.'.join(str(p) for p in err['loc'])}: {err['msg']}" for err in e.errors())


def _validate(cfg: Config) -> list[str]:
    problems = []
    if not cfg.providers:
        problems.append("at least one provider is required")
    if cfg.routing.policy not in ("cost", "latency", "quality", "rule"):
        problems.append(f"routing.policy {cfg.routing.policy!r} must be cost, latency, quality or rule")

    seen: set[str] = set()
    models: set[str] = set()  # "provider/model"
    for p in cfg.providers:
        if not p.name:
            problems.append("provider with empty name")
        if p.name in seen:
            problems.append(f"duplicate provider {p.name!r}")
        seen.add(p.name)
        if p.type in ("anthropic", "openai") and not p.api_key_env:
            problems.append(
                f"provider {p.name!r}: api_key_env is required "
                "(keys are read from the environment, never from this file)"
            )
        if not p.models:
            problems.append(f"provider {p.name!r}: no models")
        for m in p.models:
            if not m.name or m.context_window <= 0:
                problems.append(
                    f"provider {p.name!r}: every model needs a name and a positive context_window"
                )
            models.add(f"{p.name}/{m.name}")

    for rule in cfg.routing.rules:
        for key in rule.prefer:
            if key not in models:
                problems.append(f"rule {rule.name!r} prefers unknown model {key!r} (want provider/model)")
    return problems

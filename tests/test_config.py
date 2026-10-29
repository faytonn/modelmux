from pathlib import Path

import pytest

from modelmux.config import ConfigError, load, parse_duration

ROOT = Path(__file__).parent.parent


@pytest.mark.parametrize("name", ["config.yaml", "config.real.yaml"])
def test_shipped_configs_load(name: str) -> None:
    # The example configs shipped in the repo must always load.
    assert load(ROOT / name).providers


def write(tmp_path: Path, text: str) -> Path:
    path = tmp_path / "config.yaml"
    path.write_text(text, encoding="utf-8")
    return path


def test_defaults_applied(tmp_path: Path) -> None:
    cfg = load(
        write(tmp_path, "providers: [{name: m, type: mock, models: [{name: x, context_window: 1000}]}]")
    )
    assert cfg.routing.policy == "cost"
    assert cfg.retry.max_attempts == 3
    assert cfg.server.request_timeout == 30


ONE_MOCK = "providers: [{name: m, type: mock, models: [{name: x, context_window: 1}]}]"


@pytest.mark.parametrize(
    ("yaml_text", "want"),
    [
        ("providerz: []", "providerz: Extra inputs are not permitted"),
        ("server: {addr: ':1'}", "at least one provider"),
        ("routing: {policy: random}\n" + ONE_MOCK, "routing.policy"),
        (
            "providers: [{name: a, type: anthropic, models: [{name: x, context_window: 1}]}]",
            "api_key_env is required",
        ),
        (
            "routing: {policy: rule, rules: [{name: r, prefer: [nope/x]}]}\n" + ONE_MOCK,
            "unknown model 'nope/x'",
        ),
        ("retry: {base_delay: soon}\n" + ONE_MOCK, "invalid duration"),
    ],
    ids=["typo in key", "no providers", "unknown policy", "no key env", "unknown rule model", "bad duration"],
)
def test_invalid_configs(tmp_path: Path, yaml_text: str, want: str) -> None:
    with pytest.raises(ConfigError, match=want):
        load(write(tmp_path, yaml_text))


@pytest.mark.parametrize(
    ("text", "seconds"),
    [("250ms", 0.25), ("15s", 15), ("1m30s", 90), ("2h", 7200), (3, 3.0), ("0", 0.0)],
)
def test_parse_duration(text: str | int, seconds: float) -> None:
    assert parse_duration(text) == seconds

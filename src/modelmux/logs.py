"""Structured logging with a request ID on every line.

The request ID lives in a ContextVar. asyncio copies the current context into
every task it creates, so anything logged while serving a request, including
from tasks the request starts, carries that request's ID.
"""

from __future__ import annotations

import json
import logging
import secrets
import sys
from contextvars import ContextVar
from datetime import UTC, datetime

request_id_var: ContextVar[str] = ContextVar("request_id", default="")

# Attributes every LogRecord has. Anything else came from `extra=` and is
# written out as a field. uvicorn adds color_message, a copy of msg with
# terminal colour codes, which has no place in a log file.
_STANDARD_ATTRS = set(vars(logging.makeLogRecord({}))) | {"message", "asctime", "taskName", "color_message"}


def new_request_id() -> str:
    """A random 16-character hex ID."""
    return secrets.token_hex(8)


class RequestIDFilter(logging.Filter):
    """Adds request_id to every record logged while a request is being served."""

    def filter(self, record: logging.LogRecord) -> bool:
        rid = request_id_var.get()
        if rid and not hasattr(record, "request_id"):
            record.request_id = rid
        return True


class JSONFormatter(logging.Formatter):
    """One JSON object per line: time, level, msg, then the `extra=` fields."""

    def format(self, record: logging.LogRecord) -> str:
        out = {
            "time": datetime.fromtimestamp(record.created, UTC).isoformat(timespec="milliseconds"),
            "level": record.levelname,
            "msg": record.getMessage(),
        }
        for key, value in vars(record).items():
            if key not in _STANDARD_ATTRS:
                out[key] = value
        if record.exc_info:
            out["exc"] = self.formatException(record.exc_info)
        return json.dumps(out, default=str, separators=(",", ":"))


class TextFormatter(logging.Formatter):
    """key=value lines for reading logs in a terminal."""

    def format(self, record: logging.LogRecord) -> str:
        fields = " ".join(f"{k}={v}" for k, v in vars(record).items() if k not in _STANDARD_ATTRS)
        return f"{self.formatTime(record)} {record.levelname} {record.getMessage()} {fields}".rstrip()


def setup(fmt: str = "json", level: int = logging.INFO) -> logging.Logger:
    """Configures and returns the "modelmux" logger. uvicorn's own logs (startup,
    bind errors) go through the same handler, so every line has one format."""
    handler = logging.StreamHandler(sys.stdout)
    handler.setFormatter(TextFormatter() if fmt == "text" else JSONFormatter())
    handler.addFilter(RequestIDFilter())
    for name in ("modelmux", "uvicorn"):
        logger = logging.getLogger(name)
        logger.handlers[:] = [handler]
        logger.setLevel(level)
        logger.propagate = False
    return logging.getLogger("modelmux")

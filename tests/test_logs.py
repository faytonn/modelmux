import io
import json
import logging

from modelmux.logs import JSONFormatter, RequestIDFilter, new_request_id, request_id_var


def test_request_id_added_to_every_line() -> None:
    buf = io.StringIO()
    handler = logging.StreamHandler(buf)
    handler.setFormatter(JSONFormatter())
    handler.addFilter(RequestIDFilter())
    log = logging.getLogger("test_logs")
    log.handlers[:] = [handler]
    log.propagate = False

    token = request_id_var.set("abc123")
    try:
        log.warning("hello", extra={"component": "test"})
    finally:
        request_id_var.reset(token)
    log.warning("outside a request")

    first, second = (json.loads(line) for line in buf.getvalue().splitlines())
    assert first["msg"] == "hello"
    assert first["request_id"] == "abc123"
    assert first["component"] == "test"
    assert "request_id" not in second


def test_new_request_id_is_unique() -> None:
    a, b = new_request_id(), new_request_id()
    assert len(a) == 16
    assert a != b

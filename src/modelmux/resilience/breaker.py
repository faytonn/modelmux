from __future__ import annotations

import threading
import time
from collections.abc import Callable
from enum import IntEnum


class State(IntEnum):
    # The values are what the modelmux_circuit_breaker_state gauge reports.
    CLOSED = 0  # normal operation
    OPEN = 1  # failing: reject calls until the open timeout passes
    HALF_OPEN = 2  # testing recovery: allow a single probe call

    def __str__(self) -> str:
        return self.name.lower().replace("_", "-")


class BreakerOpenError(Exception):
    """A call was rejected by an open circuit breaker."""

    def __init__(self, provider: str) -> None:
        super().__init__(f"{provider}: circuit breaker open")


class Breaker:
    """A consecutive-failure circuit breaker.

    Closed -> Open after `threshold` consecutive failures.
    Open -> HalfOpen once `open_timeout` has passed; one probe call is allowed.
    HalfOpen -> Closed if the probe succeeds, back to Open if it fails.

    The gateway runs on one event loop, where these methods cannot interleave
    because none of them awaits. The lock makes the breaker safe from threads
    too, so it does not depend on how it is called.
    """

    def __init__(
        self,
        threshold: int,
        open_timeout: float,
        on_change: Callable[[State, State], None] | None = None,
        now: Callable[[], float] = time.monotonic,
    ) -> None:
        self._threshold = threshold
        self._open_timeout = open_timeout
        self._on_change = on_change
        self._now = now  # test hook

        self._lock = threading.Lock()
        self._state = State.CLOSED
        self._failures = 0
        self._opened_at = 0.0
        self._probing = False  # a half-open probe is in flight

    @property
    def state(self) -> State:
        with self._lock:
            return self._state

    def allow(self) -> bool:
        """Reports whether a call may proceed. Every allowed call must be
        followed by exactly one of success(), failure() or release()."""
        with self._lock:
            before = self._state
            allowed = False
            if self._state == State.CLOSED:
                allowed = True
            elif self._state == State.OPEN:
                if self._now() - self._opened_at >= self._open_timeout:
                    self._state, self._probing, allowed = State.HALF_OPEN, True, True
            elif not self._probing:  # half-open, probe slot free
                self._probing, allowed = True, True
            after = self._state
        self._notify(before, after)
        return allowed

    def success(self) -> None:
        """Records a call that shows the provider is healthy."""
        with self._lock:
            before = self._state
            if self._state == State.CLOSED:
                self._failures = 0
            elif self._state == State.HALF_OPEN:
                self._state, self._failures, self._probing = State.CLOSED, 0, False
            after = self._state
        self._notify(before, after)

    def failure(self) -> None:
        """Records a call that shows the provider is unhealthy."""
        with self._lock:
            before = self._state
            if self._state == State.CLOSED:
                self._failures += 1
                if self._failures >= self._threshold:
                    self._state, self._opened_at = State.OPEN, self._now()
            elif self._state == State.HALF_OPEN:
                self._state, self._opened_at, self._probing = State.OPEN, self._now(), False
            # OPEN: a slow call that started before the breaker opened; nothing to do.
            after = self._state
        self._notify(before, after)

    def release(self) -> None:
        """Ends an allowed call that says nothing about provider health (for
        example, the client cancelled). It frees the half-open probe slot."""
        with self._lock:
            if self._state == State.HALF_OPEN:
                self._probing = False

    def _notify(self, before: State, after: State) -> None:
        # Called outside the lock, so the callback can never deadlock by
        # calling back into the breaker.
        if before != after and self._on_change is not None:
            self._on_change(before, after)

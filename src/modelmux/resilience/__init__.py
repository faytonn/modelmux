"""Generic reliability building blocks: retries with backoff and a circuit
breaker. They know nothing about HTTP or LLMs; callers decide what counts as a
retryable error."""

from .breaker import Breaker, BreakerOpenError, State
from .retry import RetryPolicy, retry

__all__ = ["Breaker", "BreakerOpenError", "RetryPolicy", "State", "retry"]

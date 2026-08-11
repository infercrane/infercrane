class InferCraneError(Exception):
    """Base class for SDK failures."""


class APIError(InferCraneError):
    def __init__(self, status: int, code: str, message: str, *, retryable: bool = False, remediation: str = "") -> None:
        super().__init__(f"[{code}] {message}")
        self.status = status
        self.code = code
        self.message = message
        self.retryable = retryable
        self.remediation = remediation


class OperationFailed(InferCraneError):
    def __init__(self, operation: object) -> None:
        self.operation = operation
        code = getattr(operation, "error_code", "") or "operation_failed"
        message = getattr(operation, "message", "") or "durable operation failed"
        super().__init__(f"operation {getattr(operation, 'id', '?')} failed [{code}]: {message}")


class OperationCancelled(InferCraneError):
    def __init__(self, operation: object) -> None:
        self.operation = operation
        super().__init__(f"operation {getattr(operation, 'id', '?')} was cancelled")


class OperationTimeout(TimeoutError, InferCraneError):
    def __init__(self, operation_id: str, timeout: float) -> None:
        self.operation_id = operation_id
        self.timeout = timeout
        super().__init__(f"stopped waiting after {timeout:g}s; operation {operation_id} continues safely")


class StreamError(InferCraneError):
    pass

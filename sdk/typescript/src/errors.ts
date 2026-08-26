import type { Operation } from "./generated/models.js";

export class InferCraneError extends Error {}

export class ApiError extends InferCraneError {
  constructor(
    public readonly status: number,
    public readonly code: string,
    message: string,
    public readonly retryable = false,
    public readonly remediation = "",
  ) {
    super(`[${code}] ${message}`);
    this.name = "ApiError";
  }
}

export class OperationFailed extends InferCraneError {
  constructor(public readonly operation: Operation) {
    super(
      `operation ${operation.id} failed [${operation.error_code ?? "operation_failed"}]: ${operation.message ?? "durable operation failed"}`,
    );
    this.name = "OperationFailed";
  }
}

export class OperationCancelled extends InferCraneError {
  constructor(public readonly operation: Operation) {
    super(`operation ${operation.id} was cancelled`);
    this.name = "OperationCancelled";
  }
}

export class OperationTimeout extends InferCraneError {
  constructor(
    public readonly operationId: string,
    public readonly timeoutMs: number,
  ) {
    super(`stopped waiting after ${timeoutMs}ms; operation ${operationId} continues safely`);
    this.name = "OperationTimeout";
  }
}

export class StreamError extends InferCraneError {}

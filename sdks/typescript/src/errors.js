// Excepciones del SDK. Ninguna lleva el token.

export class SandboxError extends Error {
  constructor(message) {
    super(message);
    this.name = new.target.name;
  }
}
export class ConfigError extends SandboxError {}
export class TransportError extends SandboxError {}

/** Respuesta de error del gateway: `{"message": ..., "code": ...}`. */
export class ApiError extends SandboxError {
  /**
   * @param {number} status
   * @param {string} message
   * @param {string} [code]
   * @param {number|null} [retryAfter] segundos
   */
  constructor(status, message, code, retryAfter) {
    super(`HTTP ${status}: ${message}`);
    this.status = status;
    this.apiMessage = message;
    this.code = code;
    this.retry_after = retryAfter ?? null;
  }
}
export class BadRequest extends ApiError {}
export class Unauthorized extends ApiError {}
export class Forbidden extends ApiError {}
export class NotFound extends ApiError {}
export class Conflict extends ApiError {}
export class PayloadTooLarge extends ApiError {}
/** 429: cuota superada; `retry_after` en segundos si el servidor lo dijo. */
export class RateLimited extends ApiError {}
/** 5xx; `retry_after` se rellena en el 503 de «plantilla reconstruyéndose». */
export class ServerError extends ApiError {}

const POR_CODIGO = {
  400: BadRequest, 401: Unauthorized, 403: Forbidden, 404: NotFound,
  409: Conflict, 413: PayloadTooLarge, 429: RateLimited,
};

export function errorPara(status, message, code, retryAfter) {
  const Cls = POR_CODIGO[status] ?? (status >= 500 ? ServerError : ApiError);
  return new Cls(status, message, code, retryAfter);
}

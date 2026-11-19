"""Excepciones del SDK. Ninguna lleva el token."""

from typing import Optional


class SandboxError(Exception):
    """Base de todo lo que lanza el SDK."""


class ConfigError(SandboxError):
    """Configuración inválida (URL mal formada, etc.)."""


class TransportError(SandboxError):
    """No se pudo hablar con el gateway (red, plazo, flujo cortado)."""


class ApiError(SandboxError):
    """Respuesta de error del gateway: `{"message": ..., "code": ...}`."""

    def __init__(self, status: int, message: str, code: Optional[str] = None,
                 retry_after: Optional[float] = None):
        super().__init__("HTTP %d: %s" % (status, message))
        self.status = status
        self.message = message
        self.code = code
        self.retry_after = retry_after


class BadRequest(ApiError):
    """400."""


class Unauthorized(ApiError):
    """401: token ausente o inválido."""


class Forbidden(ApiError):
    """403."""


class NotFound(ApiError):
    """404."""


class Conflict(ApiError):
    """409."""


class PayloadTooLarge(ApiError):
    """413."""


class RateLimited(ApiError):
    """429: cuota superada. `retry_after` en segundos si el servidor lo dijo."""


class ServerError(ApiError):
    """5xx. `retry_after` se rellena en el 503 de «plantilla reconstruyéndose»."""


_POR_CODIGO = {400: BadRequest, 401: Unauthorized, 403: Forbidden, 404: NotFound,
               409: Conflict, 413: PayloadTooLarge, 429: RateLimited}


def error_para(status: int, message: str, code: Optional[str], retry_after: Optional[float]) -> ApiError:
    cls = _POR_CODIGO.get(status)
    if cls is None:
        cls = ServerError if status >= 500 else ApiError
    return cls(status, message, code, retry_after)

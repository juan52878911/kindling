"""Administración de inquilinos y claves del gateway (token de admin)."""

from typing import Any, Dict, List, Optional

from .client import Client
from .models import CreatedKey, Tenant


class Admin:
    """`/v1/tenants`. Hace falta un token de admin; si no, `Forbidden`.

    >>> adm = Admin(url="https://gateway:8090", token="...")
    >>> adm.create_tenant("ana", limits={"max_sandboxes": 10})
    >>> clave = adm.create_key("ana").key   # solo se ve ahora
    """

    def __init__(self, url: Optional[str] = None, token: Optional[str] = None,
                 timeout: Optional[float] = 60.0, client: Optional[Client] = None):
        self.client = client or Client(url=url, token=token, timeout=timeout)

    def _tenant(self, id: str) -> str:
        return "/v1/tenants/" + id

    def list_tenants(self) -> List[Tenant]:
        r = self.client._json("GET", "/v1/tenants") or {}
        return [Tenant.from_json(t) for t in r.get("tenants") or []]

    def get_tenant(self, id: str) -> Tenant:
        return Tenant.from_json(self.client._json("GET", self._tenant(id)))

    def create_tenant(self, name: str, admin: bool = False,
                      limits: Optional[Dict[str, int]] = None) -> Tenant:
        cuerpo: Dict[str, Any] = {"name": name, "admin": admin}
        if limits:
            cuerpo["limits"] = limits
        return Tenant.from_json(self.client._json("POST", "/v1/tenants", cuerpo=cuerpo))

    def update_tenant(self, id: str, admin: Optional[bool] = None,
                      limits: Optional[Dict[str, int]] = None) -> Tenant:
        """Cambia lo que se pase; `limits` sustituye todos los topes."""
        cuerpo: Dict[str, Any] = {}
        if admin is not None:
            cuerpo["admin"] = admin
        if limits is not None:
            cuerpo["limits"] = limits
        return Tenant.from_json(self.client._json("PATCH", self._tenant(id), cuerpo=cuerpo))

    def delete_tenant(self, id: str) -> None:
        self.client._json("DELETE", self._tenant(id))

    def create_key(self, tenant: str, expires_at: Optional[str] = None) -> CreatedKey:
        """`expires_at` en RFC 3339. Devuelve la clave en claro, una sola vez."""
        cuerpo = {"expires_at": expires_at} if expires_at else None
        return CreatedKey.from_json(self.client._json("POST", self._tenant(tenant) + "/keys", cuerpo=cuerpo))

    def rotate_key(self, tenant: str, key_id: str, grace_minutes: Optional[int] = None) -> CreatedKey:
        """Clave nueva; la vieja vale `grace_minutes` más (10 por defecto, 0 = ya)."""
        cuerpo = {"grace_minutes": grace_minutes} if grace_minutes is not None else None
        return CreatedKey.from_json(self.client._json(
            "POST", "%s/keys/%s/rotate" % (self._tenant(tenant), key_id), cuerpo=cuerpo))

    def revoke_key(self, tenant: str, key_id: str) -> None:
        """Revoca al instante; revocar una ya revocada no falla."""
        self.client._json("DELETE", "%s/keys/%s" % (self._tenant(tenant), key_id))

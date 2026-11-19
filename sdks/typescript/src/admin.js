// Administración de inquilinos y claves del gateway (token de admin).

import { Client } from "./client.js";

const INSPECT = Symbol.for("nodejs.util.inspect.custom");

/** Respuesta de crear o rotar una clave: `key` es la ÚNICA vez que se ve; no sale en inspect ni JSON. */
export class CreatedKey {
  #key;
  constructor(j) {
    const { key, ...resto } = j ?? {};
    Object.assign(this, resto);
    this.#key = key;
  }
  /** La clave en claro. */
  get key() { return this.#key; }
  toString() { return `CreatedKey(id=${JSON.stringify(this.id)}, key=<oculta>)`; }
  toJSON() { return { ...this, key: "<oculta>" }; }
  [INSPECT]() { return this.toString(); }
}

export class Admin {
  /** @param {{url?: string, token?: string, timeout?: number|null, fetch?: typeof fetch, client?: Client}} [opts] */
  constructor({ client, ...opts } = {}) { this.client = client ?? new Client(opts); }

  async listTenants() { return ((await this.client._json("GET", "/v1/tenants")) ?? {}).tenants ?? []; }
  getTenant(id) { return this.client._json("GET", "/v1/tenants/" + id); }
  createTenant(name, { admin = false, limits } = {}) {
    return this.client._json("POST", "/v1/tenants", { cuerpo: { name, admin, ...(limits && { limits }) } });
  }
  /** Cambia lo que se pase; `limits` sustituye todos los topes. */
  updateTenant(id, { admin, limits } = {}) {
    return this.client._json("PATCH", "/v1/tenants/" + id, {
      cuerpo: { ...(admin !== undefined && { admin }), ...(limits !== undefined && { limits }) },
    });
  }
  async deleteTenant(id) { await this.client._json("DELETE", "/v1/tenants/" + id); }

  /** `expiresAt` en RFC 3339. Devuelve la clave en claro, una sola vez. */
  async createKey(tenant, { expiresAt } = {}) {
    const cuerpo = expiresAt ? { expires_at: expiresAt } : undefined;
    return new CreatedKey(await this.client._json("POST", `/v1/tenants/${tenant}/keys`, { cuerpo }));
  }
  /** Clave nueva; la vieja vale `graceMinutes` más (10 por defecto, 0 = ya). */
  async rotateKey(tenant, keyId, { graceMinutes } = {}) {
    const cuerpo = graceMinutes !== undefined ? { grace_minutes: graceMinutes } : undefined;
    return new CreatedKey(await this.client._json("POST", `/v1/tenants/${tenant}/keys/${keyId}/rotate`, { cuerpo }));
  }
  /** Revoca al instante; revocar una ya revocada no falla. */
  async revokeKey(tenant, keyId) { await this.client._json("DELETE", `/v1/tenants/${tenant}/keys/${keyId}`); }
}

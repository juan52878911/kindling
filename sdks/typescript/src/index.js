// SDK de JavaScript para la API /v1 del gateway de sandboxes de kindling.
export { Client } from "./client.js";
export { Sandbox, Files, Processes, Sessions } from "./sandbox.js";
export {
  SandboxError, ConfigError, TransportError, ApiError, BadRequest, Unauthorized, Forbidden,
  NotFound, Conflict, PayloadTooLarge, RateLimited, ServerError,
} from "./errors.js";
export const version = "0.1.0";

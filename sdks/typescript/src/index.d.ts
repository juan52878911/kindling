export interface SandboxInfo {
  id: string; host: string; template: string; image: string; state: string;
  vcpus: number; mem_mib: number; egress: string; allow_domains?: string[];
  on_ttl: string; ttl_seconds: number; created_at: string; last_used_at: string;
  expires_at: string; labels?: Record<string, string>;
  [extra: string]: unknown;
}
export interface FileStat { path: string; size: number; mode: string; is_dir: boolean; mod_time: string; [extra: string]: unknown }
export interface Graph { id: string; host: string; template: string; state: string; claimed_at: string; nodes: Record<string, unknown>; [extra: string]: unknown }

export interface ExecResult {
  exit_code: number; stdout: string; stderr: string;
  stdoutBytes: Uint8Array; stderrBytes: Uint8Array;
  duration_ms: number; truncated: boolean; timed_out: boolean;
}
export interface ExecEvent {
  stream: "stdout" | "stderr" | null; data: Uint8Array; text: string;
  exit: number | null; duration_ms: number; truncated: boolean; timed_out: boolean; error: string | null;
}
export interface ExecOptions {
  cwd?: string; env?: Record<string, string> | string[]; /** segundos */ timeout?: number;
  stdin?: string | Uint8Array; user?: string; maxOutputBytes?: number;
  /** id de una sesión (sandbox.sessions) cuyo cwd y entorno se usan */ session?: string;
}
export type ProcessStartOptions = Omit<ExecOptions, "stdin">;
export interface ProcessInfo {
  pid: number; cmd: string[]; dir?: string; state: "running" | "exited"; started_at: string;
  ended_at?: string; exit_code?: number; signal?: string; timed_out?: boolean;
  output_bytes: number; output_start?: number; [extra: string]: unknown;
}
export interface ProcessEvent {
  stream: "stdout" | "stderr" | null; data: Uint8Array; text: string; /** siguiente al trozo: para reanudar */ offset: number;
  dropped: number; exit: number | null; signal: string; error: string | null;
}
export interface SessionInfo {
  id: string; dir?: string; env?: string[]; unset?: string[]; created_at: string; expires_at: string; execs: number;
  [extra: string]: unknown;
}
export interface SessionOptions { id?: string; cwd?: string; env?: Record<string, string> | string[]; /** segundos */ ttl?: number }
export interface ConnectionOptions {
  url?: string; token?: string; /** segundos; null = sin plazo */ timeout?: number | null; client?: Client;
}
export interface CreateOptions extends ConnectionOptions {
  template?: string; /** segundos */ ttl?: number; image?: string; on_ttl?: string; egress?: string;
  allow_domains?: string[]; mem_mib?: number; vcpus?: number; labels?: Record<string, string>;
  /** Volúmenes del inquilino a montar. */
  volumes?: VolumeMount[];
}
export interface VolumeMount { name: string; mount: string; read_only?: boolean; /** crea `name` clonando este volumen propio */ clone_from?: string }
export interface CreateManyOptions extends CreateOptions { /** todo o nada */ atomic?: boolean }
export interface BatchError { index: number; status: number; error: string }
export interface VolumeInfo { name: string; host: string; size_mib: number; used_bytes: number; in_use: boolean; [extra: string]: unknown }

export type TenantLimits = Partial<Record<"max_sandboxes" | "max_per_template" | "max_shells" | "max_processes" | "max_sessions" | "max_follow" | "max_mem_mib" | "max_vcpus" | "max_mem_total_mib" | "max_batch" | "max_volumes", number>>;
export interface TenantKey { id: string; prefix: string; created: string; expires?: string; revoked?: string; last_used?: string; [extra: string]: unknown }
export interface TenantInfo { id: string; name: string; source: "store" | "env"; read_only: boolean; admin: boolean; limits: TenantLimits; created?: string; keys: TenantKey[]; [extra: string]: unknown }

/** Respuesta de crear o rotar una clave; `key` (la clave en claro) solo se ve aquí y no sale en inspect ni JSON. */
export class CreatedKey {
  readonly id: string;
  readonly prefix: string;
  readonly created: string;
  readonly expires?: string;
  readonly old_key_id?: string;
  readonly old_key_expires?: string;
  readonly key: string;
}

/** `/v1/tenants`: inquilinos y claves del gateway. Pide un token de admin (si no, `Forbidden`). */
export class Admin {
  constructor(opts?: { url?: string; token?: string; timeout?: number | null; fetch?: typeof fetch; client?: Client });
  readonly client: Client;
  listTenants(): Promise<TenantInfo[]>;
  getTenant(id: string): Promise<TenantInfo>;
  createTenant(name: string, o?: { admin?: boolean; limits?: TenantLimits }): Promise<TenantInfo>;
  updateTenant(id: string, o?: { admin?: boolean; limits?: TenantLimits }): Promise<TenantInfo>;
  deleteTenant(id: string): Promise<void>;
  createKey(tenant: string, o?: { expiresAt?: string }): Promise<CreatedKey>;
  rotateKey(tenant: string, keyId: string, o?: { graceMinutes?: number }): Promise<CreatedKey>;
  revokeKey(tenant: string, keyId: string): Promise<void>;
}

export class Client {
  constructor(opts?: { url?: string; token?: string; timeout?: number | null; fetch?: typeof fetch });
  readonly url: string;
  health(): Promise<boolean>;
  hosts(): Promise<unknown[]>;
  templates(): Promise<unknown[]>;
  metrics(): Promise<string>;
  createSandbox(peticion: Record<string, unknown>): Promise<SandboxInfo>;
  createSandboxes(count: number, peticion?: Record<string, unknown> & { atomic?: boolean }): Promise<{ sandboxes: SandboxInfo[]; errors: BatchError[] }>;
  getSandbox(id: string): Promise<SandboxInfo>;
  listSandboxes(): Promise<{ sandboxes: SandboxInfo[]; partial: string | null }>;
  deleteSandbox(id: string): Promise<void>;
  renewSandbox(id: string, ttlSeconds?: number): Promise<SandboxInfo>;
  execWait(id: string, peticion: Record<string, unknown>): Promise<unknown>;
  execStream(id: string, peticion: Record<string, unknown>): Promise<AsyncGenerator<Record<string, unknown>>>;
  readFile(id: string, path: string): Promise<Uint8Array>;
  statFile(id: string, path: string): Promise<FileStat>;
  writeFile(id: string, path: string, datos: Uint8Array | string, o?: { mode?: number; mkdir?: boolean }): Promise<FileStat>;
  deleteFile(id: string, path: string): Promise<void>;
  createVolume(name: string, o?: { size_mib?: number; clone_from?: string; host?: string }): Promise<VolumeInfo>;
  getVolume(name: string): Promise<VolumeInfo>;
  listVolumes(): Promise<VolumeInfo[]>;
  deleteVolume(name: string): Promise<void>;
  createGraph(template: string): Promise<Graph>;
  getGraph(id: string): Promise<Graph>;
  listGraphs(): Promise<Graph[]>;
  deleteGraph(id: string): Promise<void>;
  startProcess(id: string, peticion: object): Promise<ProcessInfo>;
  listProcesses(id: string): Promise<ProcessInfo[]>;
  getProcess(id: string, pid: number, wait?: number): Promise<ProcessInfo>;
  signalProcess(id: string, pid: number, signal?: string): Promise<ProcessInfo>;
  deleteProcess(id: string, pid: number): Promise<void>;
  processOutput(id: string, pid: number, offset?: number, follow?: boolean): Promise<AsyncGenerator<Record<string, unknown>>>;
  createSession(id: string, peticion: object): Promise<SessionInfo>;
  listSessions(id: string): Promise<SessionInfo[]>;
  getSession(id: string, sid: string): Promise<SessionInfo>;
  deleteSession(id: string, sid: string): Promise<void>;
}

export class Files {
  read(path: string): Promise<Uint8Array>;
  readText(path: string): Promise<string>;
  write(path: string, data: string | Uint8Array, o?: { mode?: number; mkdir?: boolean }): Promise<FileStat>;
  delete(path: string): Promise<void>;
  stat(path: string): Promise<FileStat>;
}

export class Processes {
  start(cmd: string | string[], opts?: ProcessStartOptions): Promise<ProcessInfo>;
  list(): Promise<ProcessInfo[]>;
  get(pid: number, opts?: { /** segundos */ wait?: number }): Promise<ProcessInfo>;
  output(pid: number, opts?: { offset?: number; follow?: boolean }): AsyncGenerator<ProcessEvent>;
  signal(pid: number, signal?: string): Promise<ProcessInfo>;
  kill(pid: number): Promise<ProcessInfo>;
  delete(pid: number): Promise<void>;
}

export class Sessions {
  create(opts?: SessionOptions): Promise<SessionInfo>;
  list(): Promise<SessionInfo[]>;
  get(sid: string): Promise<SessionInfo>;
  exec(sid: string, cmd: string | string[], opts?: Omit<ExecOptions, "session">): Promise<ExecResult>;
  delete(sid: string): Promise<void>;
}

export type SandboxList = Sandbox[] & { partial: string | null };
export type SandboxBatch = Sandbox[] & { errors: BatchError[] };

export class Sandbox implements AsyncDisposable {
  constructor(info: SandboxInfo, client: Client);
  info: SandboxInfo;
  readonly id: string;
  readonly files: Files;
  readonly processes: Processes;
  readonly sessions: Sessions;
  static create(opts?: CreateOptions): Promise<Sandbox>;
  static createMany(count: number, opts?: CreateManyOptions): Promise<SandboxBatch>;
  static connect(id: string, opts?: ConnectionOptions): Promise<Sandbox>;
  static list(opts?: ConnectionOptions): Promise<SandboxList>;
  refresh(): Promise<this>;
  renew(ttl?: number): Promise<SandboxInfo>;
  delete(): Promise<void>;
  kill(): Promise<void>;
  exec(cmd: string | string[], opts?: ExecOptions): Promise<ExecResult>;
  execStream(cmd: string | string[], opts?: ExecOptions): AsyncGenerator<ExecEvent>;
  exec_stream(cmd: string | string[], opts?: ExecOptions): AsyncGenerator<ExecEvent>;
  [Symbol.asyncDispose](): Promise<void>;
}

export class SandboxError extends Error {}
export class ConfigError extends SandboxError {}
export class TransportError extends SandboxError {}
export class ApiError extends SandboxError {
  constructor(status: number, message: string, code?: string, retryAfter?: number | null);
  status: number; apiMessage: string; code?: string; retry_after: number | null;
}
export class BadRequest extends ApiError {}
export class Unauthorized extends ApiError {}
export class Forbidden extends ApiError {}
export class NotFound extends ApiError {}
export class Conflict extends ApiError {}
export class PayloadTooLarge extends ApiError {}
export class RateLimited extends ApiError {}
export class ServerError extends ApiError {}
export const version: string;

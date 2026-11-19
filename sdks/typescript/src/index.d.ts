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
}
export interface ConnectionOptions {
  url?: string; token?: string; /** segundos; null = sin plazo */ timeout?: number | null; client?: Client;
}
export interface CreateOptions extends ConnectionOptions {
  template?: string; /** segundos */ ttl?: number; image?: string; on_ttl?: string; egress?: string;
  allow_domains?: string[]; mem_mib?: number; vcpus?: number; labels?: Record<string, string>;
}

export class Client {
  constructor(opts?: { url?: string; token?: string; timeout?: number | null; fetch?: typeof fetch });
  readonly url: string;
  health(): Promise<boolean>;
  hosts(): Promise<unknown[]>;
  templates(): Promise<unknown[]>;
  metrics(): Promise<string>;
  createSandbox(peticion: Record<string, unknown>): Promise<SandboxInfo>;
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
  createGraph(template: string): Promise<Graph>;
  getGraph(id: string): Promise<Graph>;
  listGraphs(): Promise<Graph[]>;
  deleteGraph(id: string): Promise<void>;
}

export class Files {
  read(path: string): Promise<Uint8Array>;
  readText(path: string): Promise<string>;
  write(path: string, data: string | Uint8Array, o?: { mode?: number; mkdir?: boolean }): Promise<FileStat>;
  delete(path: string): Promise<void>;
  stat(path: string): Promise<FileStat>;
}

export type SandboxList = Sandbox[] & { partial: string | null };

export class Sandbox implements AsyncDisposable {
  constructor(info: SandboxInfo, client: Client);
  info: SandboxInfo;
  readonly id: string;
  readonly files: Files;
  static create(opts?: CreateOptions): Promise<Sandbox>;
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

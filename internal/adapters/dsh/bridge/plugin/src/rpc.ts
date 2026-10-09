/**
 * Minimal newline-delimited JSON-RPC 2.0 peer. Zero dependencies: Node
 * streams + StringDecoder only. One JSON value per line; `\r\n` tolerated.
 * Stdout must carry nothing but these frames.
 */

import type { Readable, Writable } from 'node:stream'
import { StringDecoder } from 'node:string_decoder'
import { ErrorCode } from './protocol.ts'

/** Hard cap on a single JSON-RPC frame, both directions (CR-9): matches the
 * Go reader's 32 MiB line cap. An oversize incoming line is discarded to the
 * next newline and an error is surfaced; an oversize outgoing frame has its
 * `raw` passenger dropped, and is dropped entirely rather than sent if still
 * too big. */
export const MAX_FRAME = 32 * 1024 * 1024

export class RpcError extends Error {
  constructor(readonly code: number, message: string, readonly data?: unknown) {
    super(message)
  }
}

type Params = Record<string, unknown> | undefined
export type RequestHandler = (method: string, params: Params) => unknown
export type NotificationHandler = (method: string, params: Params) => void

interface Pending {
  resolve(value: unknown): void
  reject(error: unknown): void
}

/** JSON.stringify that never throws: cycles and BigInts degrade instead of killing the frame. */
export function safeStringify(value: unknown): string {
  try {
    return JSON.stringify(value)
  } catch {
    const seen = new WeakSet<object>()
    return JSON.stringify(value, (_k, v: unknown) => {
      if (typeof v === 'bigint') return v.toString()
      if (typeof v === 'object' && v !== null) {
        if (seen.has(v)) return '[circular]'
        seen.add(v)
      }
      if (typeof v === 'function' || typeof v === 'symbol') return undefined
      return v
    })
  }
}

export class LineRpc {
  private buf = ''
  private readonly decoder = new StringDecoder('utf8')
  private nextId = 1
  private readonly pending = new Map<number, Pending>()
  private closed = false
  private started = false
  onRequest?: RequestHandler
  onNotification?: NotificationHandler
  onClose?: () => void
  /** Called when a frame is dropped for exceeding MAX_FRAME (either direction). */
  onFrameError?: (message: string) => void
  private dropping = false

  constructor(private readonly input: Readable, private readonly output: Writable) {}

  start(): void {
    if (this.started) return
    this.started = true
    this.input.on('data', (chunk: Buffer | string) => this.feed(chunk))
    this.input.on('end', () => this.close())
    this.input.on('error', () => this.close())
  }

  /** Feed raw bytes (exported for tests). */
  feed(chunk: Buffer | string): void {
    const s = this.buf + (typeof chunk === 'string' ? chunk : this.decoder.write(chunk))
    let start = 0
    for (;;) {
      const nl = s.indexOf('\n', start)
      if (nl < 0) break
      let line = s.slice(start, nl)
      start = nl + 1
      if (this.dropping) {
        // We were discarding an oversize frame; this newline ends it. Recover.
        this.dropping = false
        this.onFrameError?.(`incoming frame exceeded ${MAX_FRAME} bytes; discarded`)
        continue
      }
      if (line.endsWith('\r')) line = line.slice(0, -1)
      if (line.trim().length > 0) this.handleLine(line)
    }
    let rest = start === 0 ? s : s.slice(start)
    // Bound the carry buffer: a partial line past the cap means the current
    // frame is oversize. Drop what we have and keep discarding until a newline.
    if (Buffer.byteLength(rest, 'utf8') > MAX_FRAME) {
      this.dropping = true
      rest = ''
    }
    this.buf = rest
  }

  private handleLine(line: string): void {
    let msg: { id?: unknown; method?: unknown; params?: unknown; result?: unknown; error?: { code: number; message: string; data?: unknown } }
    try {
      msg = JSON.parse(line)
    } catch {
      this.write({ jsonrpc: '2.0', id: null, error: { code: ErrorCode.ParseError, message: 'parse error' } })
      return
    }
    if (msg === null || typeof msg !== 'object' || Array.isArray(msg)) {
      this.write({ jsonrpc: '2.0', id: null, error: { code: ErrorCode.InvalidRequest, message: 'invalid request' } })
      return
    }
    const params = (msg.params !== null && typeof msg.params === 'object' ? msg.params : undefined) as Params
    if (typeof msg.method === 'string') {
      if (msg.id === undefined || msg.id === null) {
        try { this.onNotification?.(msg.method, params) } catch { /* notifications never answer */ }
        return
      }
      void this.dispatch(msg.id, msg.method, params)
      return
    }
    if (typeof msg.id === 'number') {
      const p = this.pending.get(msg.id)
      if (p === undefined) return // late answer to a withdrawn request
      this.pending.delete(msg.id)
      if (msg.error !== undefined) p.reject(new RpcError(msg.error.code, msg.error.message, msg.error.data))
      else p.resolve(msg.result)
    }
  }

  private async dispatch(id: unknown, method: string, params: Params): Promise<void> {
    try {
      if (this.onRequest === undefined) throw new RpcError(ErrorCode.MethodNotFound, `method not found: ${method}`)
      const result = await this.onRequest(method, params)
      this.write({ jsonrpc: '2.0', id, result: result === undefined ? {} : result })
    } catch (error) {
      const e = error instanceof RpcError ? error : new RpcError(ErrorCode.Internal, errorMessage(error))
      this.write({ jsonrpc: '2.0', id, error: { code: e.code, message: e.message, ...(e.data === undefined ? {} : { data: e.data }) } })
    }
  }

  /** Send a request to Plexus. Aborting the signal withdraws it (the late answer is dropped). */
  request(method: string, params: unknown, signal?: AbortSignal): { id: number; promise: Promise<unknown> } {
    const id = this.nextId++
    const promise = new Promise<unknown>((resolve, reject) => {
      if (this.closed) { reject(new RpcError(ErrorCode.ShuttingDown, 'transport closed')); return }
      if (signal?.aborted) { reject(signal.reason ?? new Error('aborted')); return }
      this.pending.set(id, { resolve, reject })
      signal?.addEventListener('abort', () => {
        if (this.pending.delete(id)) reject(signal.reason ?? new Error('aborted'))
      }, { once: true })
      this.write({ jsonrpc: '2.0', id, method, params })
    })
    return { id, promise }
  }

  notify(method: string, params: unknown): void {
    this.write({ jsonrpc: '2.0', method, params })
  }

  private write(frame: unknown): void {
    if (this.closed) return
    let out = safeStringify(frame)
    if (Buffer.byteLength(out, 'utf8') > MAX_FRAME) {
      // Too big to send. The only unbounded passenger is a notification's/
      // reply's `raw` native payload: drop it and retry (CR-9).
      const f = frame as any
      if (f?.params !== undefined && f.params?.raw !== undefined) { f.params = { ...f.params, raw: undefined }; out = safeStringify(f) }
      else if (f?.result !== undefined && f.result?.raw !== undefined) { f.result = { ...f.result, raw: undefined }; out = safeStringify(f) }
    }
    if (Buffer.byteLength(out, 'utf8') > MAX_FRAME) {
      // Still oversize: never emit a frame that would break the peer's reader.
      this.onFrameError?.(`outgoing frame exceeded ${MAX_FRAME} bytes; dropped`)
      return
    }
    this.output.write(out + '\n')
  }

  close(): void {
    if (this.closed) return
    this.closed = true
    for (const p of this.pending.values()) p.reject(new RpcError(ErrorCode.ShuttingDown, 'transport closed'))
    this.pending.clear()
    this.onClose?.()
  }

  get isClosed(): boolean { return this.closed }
}

export function errorMessage(error: unknown): string {
  if (error instanceof Error) return error.message
  return String(error)
}

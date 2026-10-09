/**
 * Plexus <-> DSH bridge wire protocol constants (see ../../PLUGIN-PROTOCOL.md).
 * Newline-delimited JSON-RPC 2.0 over the plugin process' stdin/stdout.
 */

/** Wire protocol version. Bump on any incompatible change. */
export const PROTOCOL = 1

export const BRIDGE_NAME = 'plexus-dsh-bridge'

/** Replaced at build time (scripts/build.mjs); kept in sync with go/internal/adapters/dsh.BridgeVersion. */
export const BRIDGE_VERSION: string = typeof __PLEXUS_BRIDGE_VERSION__ === 'string' ? __PLEXUS_BRIDGE_VERSION__ : '0.0.0-dev'

declare const __PLEXUS_BRIDGE_VERSION__: string | undefined

/** JSON-RPC error codes. -32768..-32000 are reserved by JSON-RPC; -32000..-32099 are "server error" slots. */
export const ErrorCode = {
  ParseError: -32700,
  InvalidRequest: -32600,
  MethodNotFound: -32601,
  InvalidParams: -32602,
  Internal: -32603,
  NotInitialized: -32000,
  UnknownSession: -32001,
  ResumeFailed: -32002,
  ServiceUnavailable: -32003,
  ControlFailed: -32004,
  ProtocolMismatch: -32005,
  ShuttingDown: -32006,
  /** tools.guard missing: the guest lock and the dangerous-action gate cannot be enforced; prompts are refused. */
  SafetyGateUnavailable: -32007,
} as const

/** Plexus -> bridge requests. */
export const Method = {
  Initialize: 'plexus.initialize',
  SessionOpen: 'plexus.session.open',
  SessionClose: 'plexus.session.close',
  Prompt: 'plexus.prompt',
  Steer: 'plexus.steer',
  Cancel: 'plexus.cancel',
  Control: 'plexus.control',
  Shutdown: 'plexus.shutdown',
} as const

/** Bridge -> Plexus requests (answered exactly once by Plexus). */
export const ClientMethod = {
  Permission: 'plexus.permission',
  Question: 'plexus.question',
  /** A Plexus host tool (session.open `tools`) was called by the agent. */
  Tool: 'plexus.tool',
} as const

/** Bridge -> Plexus notifications. */
export const Notification = {
  Event: 'plexus.event',
  RequestCancelled: 'plexus.request.cancelled',
} as const

/** Normalized event kinds (mirror go internal/harness EventKind). */
export type EventKind =
  | 'text_delta' | 'message' | 'tool_use' | 'tool_result'
  | 'background' | 'final' | 'error' | 'extension'

/** One `plexus.event` notification payload. */
export interface WireEvent {
  sessionId: string
  /** Bridge turn id ("" = not tied to a Plexus prompt). */
  turnId: string
  kind: EventKind
  id: string
  parent_id?: string
  text?: string
  name?: string
  status?: string
  tool?: WireTool
  /** The native DSH object, untouched (session event, lifecycle payload, stream frame). */
  raw?: unknown
}

export interface WireTool {
  call_id?: string
  name: string
  /** Parsed tool arguments (or the raw string when they are not JSON). */
  input?: unknown
}
